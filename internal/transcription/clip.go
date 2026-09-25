package transcription

import (
	"encoding/binary"
	"math"
	"os"
	"sync"
)

// Clip is the in-memory PCM buffer for one dictation. Frames arrive from the
// browser with a sequence number and a byte offset; anything out of order is
// a frame error, never silently reordered. Samples are mono 16-bit little
// endian at the backend's preferred rate.
type Clip struct {
	sampleRate int
	mu         sync.Mutex
	pcm        []byte
	nextSeq    int64
}

// NewClip starts an empty clip at the given sample rate.
func NewClip(sampleRate int) *Clip { return &Clip{sampleRate: sampleRate} }

// Append adds one frame. seq must be the next expected sequence number and
// offset must equal the bytes received so far.
func (c *Clip) Append(seq, offset int64, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq != c.nextSeq {
		return Fail(EndFrameError, "frame %d arrived out of order (expected %d)", seq, c.nextSeq)
	}
	if offset != int64(len(c.pcm)) {
		return Fail(EndFrameError, "frame %d offset %d does not match received bytes %d", seq, offset, len(c.pcm))
	}
	if len(data)%2 != 0 {
		return Fail(EndFrameError, "frame %d is not whole 16-bit samples", seq)
	}
	c.pcm = append(c.pcm, data...)
	c.nextSeq++
	return nil
}

// DurationMS is the clip length so far.
func (c *Clip) DurationMS() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return pcmDurationMS(len(c.pcm), c.sampleRate)
}

// Bytes returns a copy of the whole clip.
func (c *Clip) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.pcm))
	copy(out, c.pcm)
	return out
}

// Window returns a copy of the trailing seconds of audio and the millisecond
// offset at which that window starts inside the clip.
func (c *Clip) Window(seconds int) ([]byte, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	windowBytes := seconds * c.sampleRate * 2
	start := 0
	if len(c.pcm) > windowBytes {
		start = len(c.pcm) - windowBytes
		start -= start % 2
	}
	out := make([]byte, len(c.pcm)-start)
	copy(out, c.pcm[start:])
	return out, pcmDurationMS(start, c.sampleRate)
}

// SampleRate reports the clip's sample rate.
func (c *Clip) SampleRate() int { return c.sampleRate }

// WriteWAV writes pcm as a mono 16-bit WAV file with owner-only permissions.
func WriteWAV(path string, sampleRate int, pcm []byte) error {
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], uint32(36+len(pcm)))
	copy(header[8:], "WAVE")
	copy(header[12:], "fmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(header[32:], 2)
	binary.LittleEndian.PutUint16(header[34:], 16)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], uint32(len(pcm)))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(header); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(pcm); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Energy reports the peak and RMS amplitude of pcm on a 0..1 scale. Silence
// detection uses both; a buffer below either configured floor is silent.
func Energy(pcm []byte) (peak, rms float64) {
	samples := len(pcm) / 2
	if samples == 0 {
		return 0, 0
	}
	var sumSquares float64
	for i := 0; i < samples; i++ {
		value := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) / 32768
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
		sumSquares += value * value
	}
	return peak, math.Sqrt(sumSquares / float64(samples))
}

func pcmDurationMS(bytes, sampleRate int) int64 {
	if sampleRate <= 0 {
		return 0
	}
	return int64(bytes/2) * 1000 / int64(sampleRate)
}
