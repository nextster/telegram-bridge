package codexworker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nextster/telegram-bridge/internal/db"
)

type Config struct {
	BaseURL  string
	Token    string
	WorkerID string
	Projects map[string]string
	Poll     time.Duration
	Client   *http.Client
	CodexBin string
}

type Worker struct {
	cfg               Config
	activeThreadState map[string]string
}

func New(cfg Config) (*Worker, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.WorkerID = strings.TrimSpace(cfg.WorkerID)
	if cfg.BaseURL == "" || cfg.Token == "" {
		return nil, errors.New("worker URL and token are required")
	}
	if cfg.WorkerID == "" {
		host, _ := os.Hostname()
		cfg.WorkerID = host
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 3 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 3 * time.Minute}
	}
	if strings.TrimSpace(cfg.CodexBin) == "" {
		cfg.CodexBin = "codex"
	}
	for slug, path := range cfg.Projects {
		cfg.Projects[strings.ToLower(strings.TrimSpace(slug))] = strings.TrimSpace(path)
	}
	return &Worker{cfg: cfg, activeThreadState: make(map[string]string)}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	nextThreadSync := time.Time{}
	for {
		if !time.Now().Before(nextThreadSync) {
			if err := w.syncThreads(ctx); err != nil {
				log.Printf("sync Codex tasks failed: %v", err)
			}
			nextThreadSync = time.Now().Add(30 * time.Second)
		}
		job, ok, err := w.claim(ctx)
		if err != nil {
			log.Printf("claim Codex job failed: %v", err)
			if !sleepContext(ctx, w.cfg.Poll) {
				return ctx.Err()
			}
			continue
		}
		if !ok {
			if !sleepContext(ctx, w.cfg.Poll) {
				return ctx.Err()
			}
			continue
		}
		if err := w.runJob(ctx, job); err != nil {
			log.Printf("Codex job %s failed: %v", job.ID, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
}

func (w *Worker) syncThreads(ctx context.Context) error {
	if err := w.syncArchived(ctx); err != nil {
		return err
	}
	snapshots, state, err := ListActiveThreadSnapshots(ctx, w.cfg.CodexBin, w.activeThreadState)
	if err != nil {
		return err
	}
	if len(snapshots) > 0 {
		var response struct {
			Created int `json:"created"`
			Updated int `json:"updated"`
			Skipped int `json:"skipped"`
		}
		status, err := w.request(ctx, http.MethodPost, "/worker/v1/thread-sync", map[string]any{"threads": snapshots}, &response)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("thread sync returned HTTP %d", status)
		}
		log.Printf("synced Codex tasks to Telegram: %d created, %d updated, %d skipped", response.Created, response.Updated, response.Skipped)
	}
	w.activeThreadState = state
	return nil
}

func (w *Worker) syncArchived(ctx context.Context) error {
	threadIDs, err := ListArchivedThreadIDs(ctx, w.cfg.CodexBin)
	if err != nil {
		return err
	}
	var response struct {
		Deleted int `json:"deleted"`
	}
	status, err := w.request(ctx, http.MethodPost, "/worker/v1/archive-sync", map[string]any{"thread_ids": threadIDs}, &response)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("archive sync returned HTTP %d", status)
	}
	if response.Deleted > 0 {
		log.Printf("deleted %d Telegram topics for archived Codex tasks", response.Deleted)
	}
	return nil
}

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return ok, err
	}
	return true, w.runJob(ctx, job)
}

func (w *Worker) runJob(ctx context.Context, job db.CodexJob) error {
	cwd := strings.TrimSpace(job.Thread.CWD)
	if cwd == "" {
		cwd = w.cfg.Projects[strings.ToLower(job.Thread.ProjectSlug)]
	}
	if cwd == "" {
		err := fmt.Sprintf("No local path configured for project %q", job.Thread.ProjectSlug)
		return w.finish(ctx, job, "", err)
	}
	info, statErr := os.Stat(cwd)
	if statErr != nil || !info.IsDir() {
		err := fmt.Sprintf("Configured project path is unavailable: %s", cwd)
		return w.finish(ctx, job, "", err)
	}

	result, runErr := RunAppServer(ctx, AppServerConfig{
		CodexBin:       w.cfg.CodexBin,
		CWD:            cwd,
		ThreadID:       job.Thread.CodexThreadID,
		Prompt:         job.Prompt,
		ApprovalPolicy: "never",
		Sandbox:        "workspace-write",
	}, func(threadID string) error {
		return w.start(ctx, job, threadID)
	})
	if runErr != nil {
		if err := w.finish(ctx, job, "", runErr.Error()); err != nil {
			return fmt.Errorf("run Codex: %v; finish job: %w", runErr, err)
		}
		return runErr
	}
	return w.finish(ctx, job, result.Text, "")
}

func ParseProjects(values []string) (map[string]string, error) {
	projects := make(map[string]string, len(values))
	for _, value := range values {
		slug, path, ok := strings.Cut(value, "=")
		slug, path = strings.ToLower(strings.TrimSpace(slug)), strings.TrimSpace(path)
		if !ok || slug == "" || path == "" {
			return nil, fmt.Errorf("invalid project mapping %q; expected slug=/absolute/path", value)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("project %q path must be absolute", slug)
		}
		projects[slug] = path
	}
	return projects, nil
}

func (w *Worker) claim(ctx context.Context) (db.CodexJob, bool, error) {
	var job db.CodexJob
	status, err := w.request(ctx, http.MethodPost, "/worker/v1/jobs/claim", map[string]string{"worker_id": w.cfg.WorkerID}, &job)
	if err != nil {
		return db.CodexJob{}, false, err
	}
	if status == http.StatusNoContent {
		return db.CodexJob{}, false, nil
	}
	if status != http.StatusOK {
		return db.CodexJob{}, false, fmt.Errorf("claim returned HTTP %d", status)
	}
	return job, true, nil
}

func (w *Worker) start(ctx context.Context, job db.CodexJob, threadID string) error {
	status, err := w.request(ctx, http.MethodPost, "/worker/v1/jobs/"+job.ID+"/start", map[string]string{
		"lease_token": job.LeaseToken, "codex_thread_id": threadID,
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("start returned HTTP %d", status)
	}
	return nil
}

func (w *Worker) finish(ctx context.Context, job db.CodexJob, result, errorText string) error {
	status, err := w.request(ctx, http.MethodPost, "/worker/v1/jobs/"+job.ID+"/finish", map[string]string{
		"lease_token": job.LeaseToken, "result": result, "error": errorText,
	}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("finish returned HTTP %d", status)
	}
	return nil
}

func (w *Worker) request(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.cfg.BaseURL+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.cfg.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("worker API HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if output != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

type AppServerConfig struct {
	CodexBin       string
	CWD            string
	ThreadID       string
	Prompt         string
	ApprovalPolicy string
	Sandbox        string
	Ephemeral      bool
}

type AppServerResult struct {
	ThreadID string
	Text     string
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func RunAppServer(ctx context.Context, cfg AppServerConfig, started func(string) error) (AppServerResult, error) {
	cmd := exec.CommandContext(ctx, cfg.CodexBin, "app-server", "--listen", "stdio://")
	cmd.Dir = cfg.CWD
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return AppServerResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return AppServerResult{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return AppServerResult{}, fmt.Errorf("start codex app-server: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	reader := bufio.NewReader(stdout)

	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]string{"name": "telegram-bridge", "title": "Telegram Bridge", "version": "1"},
	}}); err != nil {
		return AppServerResult{}, err
	}
	if _, err := waitResponse(reader, 1); err != nil {
		return AppServerResult{}, appServerError(err, stderr.String())
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return AppServerResult{}, err
	}

	method := "thread/start"
	params := map[string]any{"cwd": cfg.CWD, "approvalPolicy": cfg.ApprovalPolicy, "sandbox": cfg.Sandbox}
	if cfg.Ephemeral {
		params["ephemeral"] = true
	}
	if strings.TrimSpace(cfg.ThreadID) != "" {
		method = "thread/resume"
		params["threadId"] = cfg.ThreadID
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params}); err != nil {
		return AppServerResult{}, err
	}
	response, err := waitResponse(reader, 2)
	turnRequestID := 3
	if err != nil && method == "thread/resume" && isActiveWriterError(err) {
		method = "thread/fork"
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": method, "params": map[string]any{
			"threadId": cfg.ThreadID, "cwd": cfg.CWD, "approvalPolicy": cfg.ApprovalPolicy, "sandbox": cfg.Sandbox,
		}}); err != nil {
			return AppServerResult{}, err
		}
		response, err = waitResponse(reader, 3)
		turnRequestID = 4
	}
	if err != nil {
		return AppServerResult{}, appServerError(err, stderr.String())
	}
	var threadResponse struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(response.Result, &threadResponse); err != nil {
		return AppServerResult{}, fmt.Errorf("decode %s response: %w", method, err)
	}
	if threadResponse.Thread.ID == "" {
		return AppServerResult{}, fmt.Errorf("%s response did not contain a thread id", method)
	}
	if started != nil {
		if err := started(threadResponse.Thread.ID); err != nil {
			return AppServerResult{}, err
		}
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": turnRequestID, "method": "turn/start", "params": map[string]any{
		"threadId": threadResponse.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": cfg.Prompt}},
	}}); err != nil {
		return AppServerResult{}, err
	}
	if _, err := waitResponse(reader, turnRequestID); err != nil {
		return AppServerResult{}, appServerError(err, stderr.String())
	}

	var deltas strings.Builder
	for {
		message, err := readRPC(reader)
		if err != nil {
			return AppServerResult{}, appServerError(err, stderr.String())
		}
		switch message.Method {
		case "item/agentMessage/delta":
			var params struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal(message.Params, &params) == nil {
				deltas.WriteString(params.Delta)
			}
		case "turn/completed":
			var params struct {
				Turn struct {
					Status string `json:"status"`
					Error  any    `json:"error"`
					Items  []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"items"`
				} `json:"turn"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				return AppServerResult{}, err
			}
			text := strings.TrimSpace(deltas.String())
			for _, item := range params.Turn.Items {
				if item.Type == "agentMessage" && strings.TrimSpace(item.Text) != "" {
					text = strings.TrimSpace(item.Text)
				}
			}
			if params.Turn.Status == "failed" {
				return AppServerResult{}, fmt.Errorf("Codex turn failed: %v", params.Turn.Error)
			}
			return AppServerResult{ThreadID: threadResponse.Thread.ID, Text: text}, nil
		}
	}
}

func ListArchivedThreadIDs(ctx context.Context, codexBin string) ([]string, error) {
	if strings.TrimSpace(codexBin) == "" {
		codexBin = "codex"
	}
	cmd := exec.CommandContext(ctx, codexBin, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start codex app-server for archive sync: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	reader := bufio.NewReader(stdout)
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]string{"name": "telegram-bridge", "title": "Telegram Bridge", "version": "1"},
	}}); err != nil {
		return nil, err
	}
	if _, err := waitResponse(reader, 1); err != nil {
		return nil, appServerError(err, stderr.String())
	}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		return nil, err
	}

	var ids []string
	var cursor any
	for requestID := 2; ; requestID++ {
		params := map[string]any{"archived": true, "limit": 100, "useStateDbOnly": true}
		if cursor != nil {
			params["cursor"] = cursor
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "thread/list", "params": params}); err != nil {
			return nil, err
		}
		response, err := waitResponse(reader, requestID)
		if err != nil {
			return nil, appServerError(err, stderr.String())
		}
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &page); err != nil {
			return nil, fmt.Errorf("decode archived Codex tasks: %w", err)
		}
		for _, thread := range page.Data {
			if strings.TrimSpace(thread.ID) != "" {
				ids = append(ids, thread.ID)
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return ids, nil
		}
		cursor = *page.NextCursor
	}
}

type appServerThread struct {
	ID             string  `json:"id"`
	Name           *string `json:"name"`
	Preview        string  `json:"preview"`
	CWD            string  `json:"cwd"`
	UpdatedAt      int64   `json:"updatedAt"`
	ParentThreadID *string `json:"parentThreadId"`
	Status         struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	} `json:"status"`
	Turns []struct {
		Status string            `json:"status"`
		Items  []json.RawMessage `json:"items"`
	} `json:"turns"`
}

// ListActiveThreadSnapshots returns only tasks whose visible state changed
// since the caller's previous successful sync. It still lists the complete
// non-archived task set so removed or archived IDs fall out of nextState.
func ListActiveThreadSnapshots(ctx context.Context, codexBin string, previous map[string]string) ([]db.CodexThreadSnapshot, map[string]string, error) {
	if strings.TrimSpace(codexBin) == "" {
		codexBin = "codex"
	}
	cmd := exec.CommandContext(ctx, codexBin, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start codex app-server for task sync: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	encoder := json.NewEncoder(stdin)
	reader := bufio.NewReader(stdout)
	if err := initializeAppServer(encoder, reader); err != nil {
		return nil, nil, appServerError(err, stderr.String())
	}

	var threads []appServerThread
	var cursor any
	requestID := 2
	for {
		params := map[string]any{"archived": false, "limit": 100}
		if cursor != nil {
			params["cursor"] = cursor
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "thread/list", "params": params}); err != nil {
			return nil, nil, err
		}
		response, err := waitResponse(reader, requestID)
		requestID++
		if err != nil {
			return nil, nil, appServerError(err, stderr.String())
		}
		var page struct {
			Data       []appServerThread `json:"data"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(response.Result, &page); err != nil {
			return nil, nil, fmt.Errorf("decode active Codex tasks: %w", err)
		}
		threads = append(threads, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		cursor = *page.NextCursor
	}

	nextState := make(map[string]string, len(threads))
	var snapshots []db.CodexThreadSnapshot
	for _, thread := range threads {
		if strings.TrimSpace(thread.ID) == "" || thread.ParentThreadID != nil {
			continue
		}
		state := activeThreadState(thread)
		nextState[thread.ID] = state
		if previous[thread.ID] == state {
			continue
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": "thread/read", "params": map[string]any{
			"threadId": thread.ID, "includeTurns": true,
		}}); err != nil {
			return nil, nil, err
		}
		response, err := waitResponse(reader, requestID)
		requestID++
		if err != nil {
			return nil, nil, appServerError(err, stderr.String())
		}
		var read struct {
			Thread appServerThread `json:"thread"`
		}
		if err := json.Unmarshal(response.Result, &read); err != nil {
			return nil, nil, fmt.Errorf("decode Codex task %s: %w", thread.ID, err)
		}
		role, message := lastVisibleMessage(read.Thread)
		status := effectiveThreadStatus(thread.Status.Type, read.Thread)
		title := strings.TrimSpace(thread.Preview)
		if thread.Name != nil && strings.TrimSpace(*thread.Name) != "" {
			title = strings.TrimSpace(*thread.Name)
		}
		if title == "" {
			title = "Codex task"
		}
		snapshots = append(snapshots, db.CodexThreadSnapshot{
			CodexThreadID: thread.ID, Title: title, CWD: thread.CWD, Status: status,
			ActiveFlags: thread.Status.ActiveFlags, MessageRole: role, Message: message, UpdatedAt: thread.UpdatedAt,
		})
	}
	return snapshots, nextState, nil
}

func effectiveThreadStatus(listStatus string, thread appServerThread) string {
	if listStatus != "" && listStatus != "notLoaded" {
		return listStatus
	}
	if len(thread.Turns) == 0 {
		return "idle"
	}
	switch thread.Turns[len(thread.Turns)-1].Status {
	case "inProgress":
		return "active"
	case "failed":
		return "systemError"
	default:
		return "idle"
	}
}

func initializeAppServer(encoder *json.Encoder, reader *bufio.Reader) error {
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"clientInfo": map[string]string{"name": "telegram-bridge", "title": "Telegram Bridge", "version": "1"},
	}}); err != nil {
		return err
	}
	if _, err := waitResponse(reader, 1); err != nil {
		return err
	}
	return encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
}

func activeThreadState(thread appServerThread) string {
	name := ""
	if thread.Name != nil {
		name = *thread.Name
	}
	data, _ := json.Marshal([]any{thread.UpdatedAt, thread.CWD, name, thread.Preview, thread.Status.Type, thread.Status.ActiveFlags})
	return string(data)
}

func lastVisibleMessage(thread appServerThread) (string, string) {
	for turnIndex := len(thread.Turns) - 1; turnIndex >= 0; turnIndex-- {
		items := thread.Turns[turnIndex].Items
		for itemIndex := len(items) - 1; itemIndex >= 0; itemIndex-- {
			var item struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
					Path string `json:"path"`
					URL  string `json:"url"`
					Name string `json:"name"`
				} `json:"content"`
			}
			if json.Unmarshal(items[itemIndex], &item) != nil {
				continue
			}
			switch item.Type {
			case "agentMessage":
				if text := strings.TrimSpace(item.Text); text != "" {
					return "assistant", text
				}
			case "userMessage":
				var parts []string
				for _, content := range item.Content {
					switch content.Type {
					case "text":
						parts = append(parts, strings.TrimSpace(content.Text))
					case "image", "localImage":
						parts = append(parts, "[image]")
					case "audio", "localAudio":
						parts = append(parts, "[audio]")
					case "skill", "mention":
						if content.Name != "" {
							parts = append(parts, "[@"+content.Name+"]")
						}
					}
				}
				if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
					return "user", text
				}
			}
		}
	}
	return "", ""
}

func waitResponse(reader *bufio.Reader, id int) (rpcMessage, error) {
	for {
		message, err := readRPC(reader)
		if err != nil {
			return rpcMessage{}, err
		}
		if len(message.ID) == 0 {
			continue
		}
		var responseID int
		if json.Unmarshal(message.ID, &responseID) != nil || responseID != id {
			continue
		}
		if len(message.Error) > 0 && string(message.Error) != "null" {
			var rpcErr rpcResponseError
			if json.Unmarshal(message.Error, &rpcErr) == nil && rpcErr.Message != "" {
				return rpcMessage{}, &rpcErr
			}
			return rpcMessage{}, fmt.Errorf("JSON-RPC error: %s", message.Error)
		}
		return message, nil
	}
}

func readRPC(reader *bufio.Reader) (rpcMessage, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return rpcMessage{}, err
	}
	var message rpcMessage
	if err := json.Unmarshal(line, &message); err != nil {
		return rpcMessage{}, fmt.Errorf("decode app-server message: %w", err)
	}
	return message, nil
}

type rpcResponseError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcResponseError) Error() string {
	if e == nil {
		return "JSON-RPC error"
	}
	return fmt.Sprintf("Codex app-server error %d: %s", e.Code, e.Message)
}

func isActiveWriterError(err error) bool {
	var rpcErr *rpcResponseError
	return errors.As(err, &rpcErr) && strings.Contains(strings.ToLower(rpcErr.Message), "active writer")
}

var ansiEscapePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*(?:\x07|\x1b\\))`)

func appServerError(err error, stderr string) error {
	var rpcErr *rpcResponseError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	if value := strings.TrimSpace(ansiEscapePattern.ReplaceAllString(stderr, "")); value != "" {
		return fmt.Errorf("%w: %s", err, value)
	}
	return err
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
