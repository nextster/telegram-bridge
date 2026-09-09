package media

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type FFmpeg struct{}
type probeResult struct {
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		Type   string `json:"codec_type"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"streams"`
}

const allowedFormats = "ogg,mov,matroska,webm,mp3,wav,jpeg_pipe,png_pipe,webp_pipe,image2"

func probe(ctx context.Context, path string) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", allowedFormats, "-show_entries", "format=duration:stream=codec_type,width,height", "-of", "json", path)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{target: &out, remaining: 64 << 10}
	if cmd.Run() != nil {
		return probeResult{}, Fail("invalid_media_or_probe_failed")
	}
	var p probeResult
	if json.Unmarshal(out.Bytes(), &p) != nil {
		return p, Fail("invalid_media")
	}
	return p, nil
}

func (FFmpeg) Prepare(ctx context.Context, input, dir string, a Attachment, maxSeconds int) (Prepared, error) {
	p, err := probe(ctx, input)
	if err != nil {
		return Prepared{}, err
	}
	if a.Kind == "photo" || a.Kind == "image" {
		if a.Size > 8<<20 || len(p.Streams) != 1 {
			return Prepared{}, Fail("image_size_limit")
		}
		v := p.Streams[0]
		if v.Width <= 0 || v.Height <= 0 || v.Width > 8192 || v.Height > 8192 || int64(v.Width)*int64(v.Height) > 20_000_000 {
			return Prepared{}, Fail("image_pixel_limit")
		}
		data, err := os.ReadFile(input)
		if err != nil {
			return Prepared{}, Fail("file_unavailable")
		}
		mime := http.DetectContentType(data)
		if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" {
			return Prepared{}, Fail("unsupported_image_format")
		}
		return Prepared{Data: data, MIME: mime}, nil
	}
	audio := false
	for _, stream := range p.Streams {
		audio = audio || stream.Type == "audio"
	}
	if !audio {
		return Prepared{}, Fail("audio_track_missing")
	}
	if duration, _ := strconv.ParseFloat(p.Format.Duration, 64); duration > float64(maxSeconds) {
		return Prepared{}, Fail("duration_limit")
	}
	output := filepath.Join(dir, "audio.mp3")
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe", "-format_whitelist", allowedFormats, "-i", input,
		"-map", "0:a:0", "-vn", "-map_metadata", "-1", "-ac", "1", "-ar", "16000", "-c:a", "libmp3lame", "-b:a", "48k", "-threads", "1", "-t", strconv.Itoa(maxSeconds+1), "-f", "mp3", "-y", output)
	if cmd.Run() != nil {
		return Prepared{}, Fail("audio_conversion_failed")
	}
	converted, err := probe(ctx, output)
	if err != nil {
		return Prepared{}, err
	}
	duration, err := strconv.ParseFloat(converted.Format.Duration, 64)
	// MP3 encoder padding is included in both the limit and the budget reservation.
	if err != nil || duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) || duration > float64(maxSeconds) {
		return Prepared{}, Fail("duration_limit")
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() <= 0 || info.Size() > 8<<20 {
		return Prepared{}, Fail("audio_size_limit")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		return Prepared{}, Fail("file_unavailable")
	}
	return Prepared{Data: data, MIME: "audio/mpeg", Duration: duration}, nil
}
