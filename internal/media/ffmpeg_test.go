package media

import (
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFFmpegVoiceAndVideoNoteExtraction(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	for _, kind := range []string{"voice", "video_note"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "input.ogg")
			args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "libopus", input}
			if kind == "video_note" {
				input = filepath.Join(dir, "input.mp4")
				args = []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=black:s=64x64:d=2", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:v", "mpeg4", "-c:a", "aac", "-shortest", input}
			}
			if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, output)
			}
			prepared, err := (FFmpeg{}).Prepare(context.Background(), input, dir, Attachment{Kind: kind}, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(prepared.Data) == 0 || prepared.Duration < 1.9 || prepared.Duration > 2.5 || prepared.MIME != "audio/mpeg" {
				t.Fatal("empty or incorrectly framed audio")
			}
			if _, err := (FFmpeg{}).Prepare(context.Background(), input, dir, Attachment{Kind: kind}, 1); err == nil {
				t.Fatal("actual duration limit ignored")
			}
		})
	}
}

func TestFFmpegImageAndInvalidMedia(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "original.bin")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 20, 10))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, _ := os.Stat(input)
	prepared, err := (FFmpeg{}).Prepare(context.Background(), input, dir, Attachment{Kind: "photo", Size: info.Size()}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.MIME != "image/png" || len(prepared.Data) != int(info.Size()) {
		t.Fatal("image changed")
	}
	if _, err := (FFmpeg{}).Prepare(context.Background(), input, dir, Attachment{Kind: "voice"}, 10); err == nil {
		t.Fatal("missing audio accepted")
	}
	if err := os.WriteFile(input, []byte("not a media file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FFmpeg{}).Prepare(context.Background(), input, dir, Attachment{Kind: "voice"}, 10); err == nil {
		t.Fatal("invalid media accepted")
	}
}
