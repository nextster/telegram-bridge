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
	cfg Config
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
		cfg.Client = &http.Client{Timeout: 45 * time.Second}
	}
	if strings.TrimSpace(cfg.CodexBin) == "" {
		cfg.CodexBin = "codex"
	}
	for slug, path := range cfg.Projects {
		cfg.Projects[strings.ToLower(strings.TrimSpace(slug))] = strings.TrimSpace(path)
	}
	return &Worker{cfg: cfg}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for {
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

func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	job, ok, err := w.claim(ctx)
	if err != nil || !ok {
		return ok, err
	}
	return true, w.runJob(ctx, job)
}

func (w *Worker) runJob(ctx context.Context, job db.CodexJob) error {
	cwd := w.cfg.Projects[strings.ToLower(job.Thread.ProjectSlug)]
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
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "turn/start", "params": map[string]any{
		"threadId": threadResponse.Thread.ID,
		"input":    []map[string]string{{"type": "text", "text": cfg.Prompt}},
	}}); err != nil {
		return AppServerResult{}, err
	}
	if _, err := waitResponse(reader, 3); err != nil {
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

func appServerError(err error, stderr string) error {
	if value := strings.TrimSpace(stderr); value != "" {
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
