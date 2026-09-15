package media

import (
	"context"
	"time"
)

const MaxBatchItems = 100
const MaxWait = 8 * time.Minute

type Reference struct {
	Chat      string `json:"chat" jsonschema:"Exact chat key returned by Bridge"`
	MessageID int    `json:"message_id" jsonschema:"Exact message ID returned by Bridge"`
}

type JobReference struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	JobID     string `json:"job_id"`
}

type BatchOptions struct {
	Audio Options `json:"audio,omitempty"`
	Image Options `json:"image,omitempty"`
}

type BatchItem struct {
	Chat      string `json:"chat"`
	MessageID int    `json:"message_id"`
	Job       *Job   `json:"job,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

type Batch struct {
	Items        []BatchItem `json:"items"`
	Settled      bool        `json:"settled"`
	AllSucceeded bool        `json:"all_succeeded"`
	TimedOut     bool        `json:"timed_out"`
}

func (s *Service) StartBatch(ctx context.Context, accountID int64, refs []Reference, options BatchOptions, confirmPaid bool) (Batch, error) {
	if !confirmPaid {
		return Batch{}, Fail("explicit_paid_confirmation_required")
	}
	if len(refs) < 1 || len(refs) > MaxBatchItems {
		return Batch{}, Fail("batch_size_limit")
	}
	// Validate the entire input before creating any jobs.
	for _, ref := range refs {
		if err := ValidateReference(ref.Chat, ref.MessageID); err != nil {
			return Batch{}, err
		}
	}
	var err error
	options.Audio, err = normalizeOptions(options.Audio, s.cfg.AudioModel, "transcription")
	if err != nil {
		return Batch{}, err
	}
	options.Image, err = normalizeOptions(options.Image, s.cfg.ImageModel, "image")
	if err != nil {
		return Batch{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	batch := Batch{Items: make([]BatchItem, 0, len(refs))}
	seen := make(map[Reference]BatchItem, len(refs))
	for _, ref := range refs {
		if item, ok := seen[ref]; ok {
			batch.Items = append(batch.Items, item)
			continue
		}
		item := BatchItem{Chat: ref.Chat, MessageID: ref.MessageID}
		a, err := s.Metadata(ctx, accountID, ref.Chat, ref.MessageID)
		if err == nil {
			operation, settings := "", options.Audio
			switch a.Kind {
			case "voice", "video_note":
				operation = "transcription"
			case "photo", "image":
				operation, settings = "image", options.Image
			default:
				err = Fail("unsupported_attachment")
			}
			if err == nil {
				job, startErr := s.enqueue(ctx, a, operation, settings)
				err = startErr
				if err == nil {
					item.Job = &job
				}
			}
		}
		if err != nil {
			item.ErrorCode = fault(err).Code
		}
		seen[ref] = item
		batch.Items = append(batch.Items, item)
	}
	batch.summarize()
	return batch, nil
}

func (s *Service) GetBatch(ctx context.Context, accountID int64, refs []JobReference) (Batch, error) {
	if len(refs) < 1 || len(refs) > MaxBatchItems {
		return Batch{}, Fail("batch_size_limit")
	}
	batch := Batch{Items: make([]BatchItem, 0, len(refs))}
	for _, ref := range refs {
		job, err := s.Get(ctx, accountID, ref.Chat, ref.MessageID, ref.JobID)
		if err != nil {
			return Batch{}, err
		}
		batch.Items = append(batch.Items, BatchItem{Chat: ref.Chat, MessageID: ref.MessageID, Job: &job})
	}
	batch.summarize()
	return batch, nil
}

// Waiting only observes durable jobs. Cancellation/disconnection never cancels
// shared paid work or starts a replacement request.
func (s *Service) WaitBatch(ctx context.Context, accountID int64, batch Batch, wait time.Duration) (Batch, error) {
	if wait < 0 || wait > MaxWait || len(batch.Items) > MaxBatchItems {
		return Batch{}, Fail("invalid_wait")
	}
	batch.Items = append([]BatchItem{}, batch.Items...)
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		seen := make(map[string]*Job)
		for i := range batch.Items {
			item := &batch.Items[i]
			if item.Job == nil || terminal(item.Job.Status) {
				continue
			}
			if job, ok := seen[item.Job.ID]; ok {
				item.Job = job
				continue
			}
			if ctx.Err() != nil {
				break
			}
			job, err := s.Get(ctx, accountID, item.Chat, item.MessageID, item.Job.ID)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				return Batch{}, err
			}
			item.Job = &job
			seen[job.ID] = &job
		}
		batch.summarize()
		if batch.Settled || wait == 0 {
			return batch, nil
		}
		select {
		case <-ctx.Done():
			batch.TimedOut = true
			return batch, nil
		case <-ticker.C:
		}
	}
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "uncertain"
}

func (b *Batch) summarize() {
	b.Settled, b.AllSucceeded = true, true
	for _, item := range b.Items {
		if item.Job == nil {
			b.AllSucceeded = false
			continue
		}
		if !terminal(item.Job.Status) {
			b.Settled = false
		}
		if item.Job.Status != "completed" {
			b.AllSucceeded = false
		}
	}
}
