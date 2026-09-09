package media

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nextster/telegram-bridge/internal/config"
	"github.com/nextster/telegram-bridge/internal/db"
)

type fakeSource struct {
	attachment Attachment
	downloads  int
	data       string
	err        error
	account    int64
}

func (f *fakeSource) AccountID() int64 { return f.account }
func (f *fakeSource) Attachment(context.Context, string, int) (Attachment, error) {
	return f.attachment, f.err
}
func (f *fakeSource) Download(_ context.Context, _ Attachment, w io.Writer) error {
	f.downloads++
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(w, f.data)
	return err
}

type fakeProcessor struct{ err error }

func (p fakeProcessor) Prepare(_ context.Context, _, _ string, a Attachment, _ int) (Prepared, error) {
	return Prepared{Data: []byte("normalized"), MIME: "audio/mpeg", Duration: a.Duration}, p.err
}

type fakeProvider struct {
	calls  int
	result Result
	err    error
}

func (p *fakeProvider) Recognize(context.Context, string, Options, Prepared) (Result, error) {
	p.calls++
	return p.result, p.err
}

func testService(t *testing.T) (*Service, *fakeSource, *fakeProvider) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := config.DefaultMediaConfig(path)
	cfg.Enabled = true
	cfg.APIKey = "fake-not-a-key"
	source := &fakeSource{attachment: Attachment{AccountID: 1, Chat: "channel:42", MessageID: 7, Author: "Speaker", Date: time.Unix(1700000000, 0).UTC(), ReplyToID: 6, MessageURL: "https://t.me/c/42/7", Kind: "voice", MIME: "audio/ogg", Size: 5, Duration: 10, Supported: true, Fingerprint: "original"}, data: "audio", account: 1}
	s, err := New(cfg, store, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return time.Unix(1800000000, 0) }
	provider := &fakeProvider{result: Result{Text: "verbatim words", Languages: []string{"en"}, LanguageSource: "provider"}}
	s.processor = fakeProcessor{}
	s.provider = provider
	return s, source, provider
}
func startTestJob(t *testing.T, s *Service, operation string, options Options) Job {
	t.Helper()
	j, err := s.Start(context.Background(), "channel:42", 7, operation, options, true)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func runTestJob(t *testing.T, s *Service) {
	t.Helper()
	worked, err := s.runOne(context.Background())
	if err != nil || !worked {
		t.Fatalf("runOne: worked=%v err=%v", worked, err)
	}
}
func getTestJob(t *testing.T, s *Service, id string) Job {
	t.Helper()
	j, err := s.Get(context.Background(), "channel:42", 7, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestExplicitPaidActionAndReadOnlyDownload(t *testing.T) {
	s, source, p := testService(t)
	if _, err := s.Start(context.Background(), "channel:42", 7, "transcription", Options{}, false); err == nil {
		t.Fatal("accepted implicit paid processing")
	}
	if _, err := s.Metadata(context.Background(), "channel:42", 7); err != nil {
		t.Fatal(err)
	}
	a, err := s.Download(context.Background(), "channel:42", 7)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Download(context.Background(), "channel:42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if a.FileID != b.FileID || source.downloads != 1 || p.calls != 0 {
		t.Fatal("download cache or paid boundary broken")
	}
	f, _, err := s.OpenDownload(context.Background(), a.FileID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	source.account = 2
	if _, _, err := s.OpenDownload(context.Background(), a.FileID); err == nil {
		t.Fatal("cross-account download allowed")
	}
}

func TestJobDedupAndCacheInvalidation(t *testing.T) {
	s, source, p := testService(t)
	j := startTestJob(t, s, "transcription", Options{Keywords: []string{"timeline", "Realize"}})
	same := startTestJob(t, s, "transcription", Options{Keywords: []string{"Realize", "timeline", "Realize"}})
	if same.ID != j.ID {
		t.Fatal("normalized hints not deduplicated")
	}
	runTestJob(t, s)
	complete := getTestJob(t, s, j.ID)
	if complete.Status != "completed" || complete.Result.Text != "verbatim words" || complete.SHA256 == "" || complete.Source.ReplyToID != 6 || complete.Duration != 10 {
		t.Fatalf("bad result: %+v", complete)
	}
	source.attachment.Author = "Renamed"
	same = startTestJob(t, s, "transcription", Options{Keywords: []string{"timeline", "Realize"}})
	if same.ID != j.ID || same.Status != "completed" || p.calls != 1 {
		t.Fatal("repeated request charges again")
	}
	changed := startTestJob(t, s, "transcription", Options{Keywords: []string{"different"}})
	if changed.ID == j.ID {
		t.Fatal("hint change did not invalidate cache")
	}
	source.attachment.Fingerprint = "new-audio"
	if startTestJob(t, s, "transcription", Options{}).ID == changed.ID {
		t.Fatal("source change ignored")
	}
	s.cfg.CacheRevision = "2"
	if startTestJob(t, s, "transcription", Options{}).ID == j.ID {
		t.Fatal("revision change ignored")
	}
	if _, err := s.Start(context.Background(), "channel:42", 7, "transcription", Options{Model: "unapproved/model"}, true); err == nil {
		t.Fatal("unapproved model allowed")
	}
}

func TestConcurrentDuplicateRequestsCreateOneJob(t *testing.T) {
	s, _, p := testService(t)
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			j, err := s.Start(context.Background(), "channel:42", 7, "transcription", Options{}, true)
			ids <- j.ID
			errs <- err
		})
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate jobs")
		}
	}
	runTestJob(t, s)
	if worked, err := s.runOne(context.Background()); err != nil || worked || p.calls != 1 {
		t.Fatal("duplicate paid call")
	}
}

func TestImageDescriptionAndOCRStoredSeparately(t *testing.T) {
	s, source, p := testService(t)
	source.attachment.Kind = "photo"
	source.attachment.MIME = "image/jpeg"
	source.attachment.Duration = 0
	p.result = Result{Description: "A whiteboard", OCRText: "Realize\ntimeline", Languages: []string{"en"}, LanguageSource: "provider"}
	j := startTestJob(t, s, "image", Options{})
	runTestJob(t, s)
	row, err := s.store.MediaJob(context.Background(), j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.ImageDescription != "A whiteboard" || row.ImageText != "Realize\ntimeline" || row.Transcript != "" {
		t.Fatal("image fields mixed in storage")
	}
	if strings.Contains(row.Payload, "whiteboard") || strings.Contains(row.ResultMeta, "timeline") {
		t.Fatal("result duplicated into payload/meta")
	}
	result := getTestJob(t, s, j.ID).Result
	if result.Description != row.ImageDescription || result.OCRText != row.ImageText {
		t.Fatal("stored result not returned")
	}
	if startTestJob(t, s, "image", Options{}).ID != j.ID || p.calls != 1 {
		t.Fatal("image was not cached")
	}
}

func TestRecoveryAcrossDatabaseReopen(t *testing.T) {
	s, source, p := testService(t)
	j := startTestJob(t, s, "transcription", Options{})
	if _, err := s.store.ClaimMedia(context.Background(), 1, s.now()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(s.cfg.Directory), "test.db")
	s.Close()
	s.store.Close()
	store, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restarted, err := New(s.cfg, store, source)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.now = s.now
	restarted.processor = fakeProcessor{}
	restarted.provider = p
	if getTestJob(t, restarted, j.ID).Status != "queued" {
		t.Fatal("pre-submission job not recovered")
	}
	runTestJob(t, restarted)
	if p.calls != 1 || getTestJob(t, restarted, j.ID).Status != "completed" {
		t.Fatal("recovered job not completed")
	}
	if startTestJob(t, restarted, "transcription", Options{}).ID != j.ID {
		t.Fatal("lost cache across restart")
	}
}

func TestSubmittingRecoveryNeverResubmits(t *testing.T) {
	s, _, p := testService(t)
	j := startTestJob(t, s, "transcription", Options{})
	row, err := s.store.ClaimMedia(context.Background(), 1, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.ReserveMedia(context.Background(), j.ID, row.Payload, 100, 10000, 10000, s.now()); err != nil {
		t.Fatal(err)
	}
	if err := s.store.RecoverMedia(context.Background(), s.now()); err != nil {
		t.Fatal(err)
	}
	if getTestJob(t, s, j.ID).Status != "uncertain" {
		t.Fatal("lost submission was requeued")
	}
	if worked, err := s.runOne(context.Background()); worked || err != nil || p.calls != 0 {
		t.Fatal("uncertain call was retried")
	}
	if startTestJob(t, s, "transcription", Options{}).Status != "uncertain" {
		t.Fatal("repeated request reset uncertain job")
	}
}

func TestRetriesAndUncertainOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
		calls  int
	}{
		{"rate_limit", Retry("openrouter_rate_limited", time.Second), "failed", 3},
		{"timeout", &Fault{Code: "provider_outcome_unknown", Uncertain: true}, "uncertain", 1},
		{"auth", Fail("openrouter_auth_error"), "failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, p := testService(t)
			p.err = tc.err
			j := startTestJob(t, s, "transcription", Options{})
			for n := 0; n < tc.calls; n++ {
				runTestJob(t, s)
				now := s.now().Add(time.Hour)
				s.now = func() time.Time { return now }
			}
			got := getTestJob(t, s, j.ID)
			if got.Status != tc.status || p.calls != tc.calls || got.ProviderAttempts != tc.calls {
				t.Fatalf("unexpected state: %+v calls=%d", got, p.calls)
			}
			if worked, _ := s.runOne(context.Background()); worked {
				t.Fatal("terminal job retried")
			}
		})
	}
}

func TestLimitsAndSourceChangesBeforePaidRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Service, *fakeSource)
		code   string
	}{
		{"budget", func(s *Service, _ *fakeSource) { s.cfg.DailyBudgetMicros = 1 }, "budget_exceeded"},
		{"source", func(_ *Service, f *fakeSource) { f.attachment.Fingerprint = "changed" }, "source_changed"},
		{"download_too_large", func(_ *Service, f *fakeSource) { f.data = "oversized" }, "attachment_size_limit"},
		{"bad_media", func(s *Service, _ *fakeSource) { s.processor = fakeProcessor{err: Fail("invalid_media")} }, "invalid_media"},
		{"disk", func(s *Service, _ *fakeSource) { s.cfg.MaxDiskBytes = 1 }, "file_storage_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, p := testService(t)
			j := startTestJob(t, s, "transcription", Options{})
			tc.change(s, f)
			runTestJob(t, s)
			got := getTestJob(t, s, j.ID)
			if got.ErrorCode != tc.code || p.calls != 0 {
				t.Fatalf("got %s, paid calls %d", got.ErrorCode, p.calls)
			}
		})
	}
}

func TestReferenceAndHintValidation(t *testing.T) {
	s, f, _ := testService(t)
	for _, chat := range []string{"https://example.com/audio", "/etc/passwd", "channel:1/../../etc", "channel:0", "channel:+42", "file:42"} {
		if _, err := s.Metadata(context.Background(), chat, 7); err == nil {
			t.Fatalf("accepted %q", chat)
		}
	}
	for _, term := range []string{"bad\nterm", "<instruction>", strings.Repeat("x", 81), "\x00", ""} {
		if _, err := normalizeOptions(Options{Keywords: []string{term}}, s.cfg.AudioModel, "transcription"); err == nil {
			t.Fatal("accepted invalid hint")
		}
	}
	f.attachment.Supported = false
	if _, err := s.Start(context.Background(), "channel:42", 7, "transcription", Options{}, true); err == nil {
		t.Fatal("accepted unsupported media")
	}
	f.attachment.Supported = true
	f.attachment.Duration = 601
	if _, err := s.Start(context.Background(), "channel:42", 7, "transcription", Options{}, true); err == nil {
		t.Fatal("accepted excessive duration")
	}
	f.attachment.Duration = 10
	f.attachment.Size = s.cfg.MaxBytes + 1
	if _, err := s.Download(context.Background(), "channel:42", 7); err == nil {
		t.Fatal("accepted excessive size")
	}
}

func TestCleanupExpiryOrphansAndIntegrity(t *testing.T) {
	s, _, _ := testService(t)
	d, err := s.Download(context.Background(), "channel:42", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.cfg.Directory, d.FileID+".bin"), []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Download(context.Background(), "channel:42", 7); err == nil {
		t.Fatal("corrupted cache accepted")
	}
	if err := os.WriteFile(filepath.Join(s.cfg.Directory, "tmp-orphan"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	orphan := strings.Repeat("a", 64) + ".bin"
	if err := os.WriteFile(filepath.Join(s.cfg.Directory, orphan), []byte("orphan"), 0600); err != nil {
		t.Fatal(err)
	}
	later := s.now().Add(25 * time.Hour)
	s.now = func() time.Time { return later }
	if _, _, err := s.OpenDownload(context.Background(), d.FileID); err == nil {
		t.Fatal("expired file accepted")
	}
	if err := s.cleanup(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.cfg.Directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("cleanup left %v: %v", entries, err)
	}
	if _, err := s.store.MediaFile(context.Background(), d.FileID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("expired metadata retained")
	}
}
