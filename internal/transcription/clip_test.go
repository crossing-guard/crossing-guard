package transcription

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClipRejectsGapsDuplicatesAndOddFrames(t *testing.T) {
	clip := NewClip(16000)
	frame := tonePCM(16000, 100, 0.5)
	if err := clip.Append(0, 0, frame); err != nil {
		t.Fatal(err)
	}
	var typed *Error
	if err := clip.Append(2, int64(len(frame)), frame); !errors.As(err, &typed) || typed.State != EndFrameError {
		t.Fatalf("gap should be a frame error, got %v", err)
	}
	if err := clip.Append(1, 0, frame); !errors.As(err, &typed) || typed.State != EndFrameError {
		t.Fatalf("wrong offset should be a frame error, got %v", err)
	}
	if err := clip.Append(1, int64(len(frame)), frame[:3]); !errors.As(err, &typed) {
		t.Fatalf("odd byte count should be a frame error, got %v", err)
	}
	if err := clip.Append(1, int64(len(frame)), frame); err != nil {
		t.Fatal(err)
	}
	if got := clip.DurationMS(); got != 200 {
		t.Fatalf("duration = %d", got)
	}
}

func TestClipWindowReturnsTrailingAudioWithItsOffset(t *testing.T) {
	clip := NewClip(16000)
	if err := clip.Append(0, 0, tonePCM(16000, 5000, 0.5)); err != nil {
		t.Fatal(err)
	}
	window, startMS := clip.Window(2)
	if len(window) != 2*16000*2 || startMS != 3000 {
		t.Fatalf("window bytes=%d start=%d", len(window), startMS)
	}
	whole, startMS := clip.Window(10)
	if len(whole) != 5*16000*2 || startMS != 0 {
		t.Fatalf("whole clip window bytes=%d start=%d", len(whole), startMS)
	}
}

func TestEnergyDistinguishesSilenceFromTone(t *testing.T) {
	peak, rms := Energy(silencePCM(16000, 500))
	if peak != 0 || rms != 0 {
		t.Fatalf("silence energy = %v %v", peak, rms)
	}
	peak, rms = Energy(tonePCM(16000, 500, 0.5))
	if peak < 0.45 || peak > 0.55 || rms < 0.3 || rms > 0.4 {
		t.Fatalf("tone energy = %v %v", peak, rms)
	}
}

func TestWriteWAVProducesAValidMonoHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.wav")
	pcm := tonePCM(16000, 100, 0.3)
	if err := WriteWAV(path, 16000, pcm); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" || binary.LittleEndian.Uint16(raw[22:]) != 1 ||
		binary.LittleEndian.Uint32(raw[24:]) != 16000 || int(binary.LittleEndian.Uint32(raw[40:])) != len(pcm) {
		t.Fatalf("bad header: % x", raw[:44])
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("clip permissions = %v", info.Mode().Perm())
	}
}
