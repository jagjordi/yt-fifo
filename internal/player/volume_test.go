package player

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"testing"
)

func samples(vals ...int16) []byte {
	b := make([]byte, 2*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func TestApplyGain(t *testing.T) {
	buf := samples(1000, -1000, math.MaxInt16, math.MinInt16, 1)
	applyGain(buf, 0.5)
	if want := samples(500, -500, 16384, -16384, 1); !bytes.Equal(buf, want) {
		t.Errorf("gain 0.5: got %v want %v", buf, want)
	}
	buf = samples(1000, -1000)
	applyGain(buf, 0)
	if want := samples(0, 0); !bytes.Equal(buf, want) {
		t.Errorf("gain 0: got %v", buf)
	}
	buf = samples(1000, -1000)
	applyGain(buf, 1)
	if want := samples(1000, -1000); !bytes.Equal(buf, want) {
		t.Errorf("gain 1 must not touch the samples: got %v", buf)
	}
}

// chunkReader hands out at most n bytes per Read, so the pump sees reads that
// end in the middle of a sample.
type chunkReader struct {
	r io.Reader
	n int
}

func (c chunkReader) Read(p []byte) (int, error) {
	if len(p) > c.n {
		p = p[:c.n]
	}
	return c.r.Read(p)
}

func TestPumpAppliesGainAcrossOddReads(t *testing.T) {
	var out bytes.Buffer
	p := New(Config{Rate: 48000, Channels: 2, Sink: &out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Volume: 0.5})
	in := samples(100, -200, 300, -400, 500, -600, 700)
	n, err := p.pump(chunkReader{bytes.NewReader(in), 3})
	if err != nil {
		t.Fatal(err)
	}
	want := samples(50, -100, 150, -200, 250, -300, 350)
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("got %v want %v", out.Bytes(), want)
	}
	if n != int64(len(want)) || p.bytes.Load() != int64(len(want)) {
		t.Errorf("counted %d / %d bytes, want %d", n, p.bytes.Load(), len(want))
	}
}

func TestMaxVolumeScalesWithoutChangingVolume(t *testing.T) {
	var out bytes.Buffer
	p := New(Config{Rate: 48000, Channels: 2, Sink: &out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxVolume: 0.5})
	if p.Status().Volume != 1 {
		t.Errorf("volume reported %v, want 1: the scaling must stay invisible", p.Status().Volume)
	}
	if _, err := p.pump(bytes.NewReader(samples(1000, -1000))); err != nil {
		t.Fatal(err)
	}
	if want := samples(500, -500); !bytes.Equal(out.Bytes(), want) {
		t.Errorf("max volume 0.5: got %v want %v", out.Bytes(), want)
	}
	out.Reset()
	p.SetVolume(0.5)
	if _, err := p.pump(bytes.NewReader(samples(1000, -1000))); err != nil {
		t.Fatal(err)
	}
	if want := samples(250, -250); !bytes.Equal(out.Bytes(), want) {
		t.Errorf("max volume 0.5 x volume 0.5: got %v want %v", out.Bytes(), want)
	}
}

func TestApplyDither(t *testing.T) {
	rng := uint64(12345)
	buf := samples(0, 0, 1000, -1000)
	applyDither(buf, 0, &rng)
	if want := samples(0, 0, 1000, -1000); !bytes.Equal(buf, want) {
		t.Errorf("amp 0 must not touch the samples: got %v", buf)
	}
	for _, amp := range []int{1, 16} {
		buf = make([]byte, 2*10000)
		applyDither(buf, amp, &rng)
		var sum, nonzero int
		for i := 0; i < len(buf); i += 2 {
			v := int(int16(binary.LittleEndian.Uint16(buf[i:])))
			if v < -amp || v > amp {
				t.Fatalf("amp %d: sample %d out of range", amp, v)
			}
			if v != 0 {
				nonzero++
			}
			sum += v
		}
		if nonzero < 2000 {
			t.Errorf("amp %d: only %d of 10000 samples non-zero", amp, nonzero)
		}
		if mean := float64(sum) / 10000; mean > 0.5 || mean < -0.5 {
			t.Errorf("amp %d: mean %v, want ~0", amp, mean)
		}
	}
	buf = samples(math.MaxInt16, math.MinInt16)
	applyDither(buf, 16, &rng) // must clamp, not wrap
	if v := int16(binary.LittleEndian.Uint16(buf)); v < math.MaxInt16-16 {
		t.Errorf("max sample wrapped to %d", v)
	}
	if v := int16(binary.LittleEndian.Uint16(buf[2:])); v > math.MinInt16+16 {
		t.Errorf("min sample wrapped to %d", v)
	}
}

func TestPumpDithersAfterGain(t *testing.T) {
	var out bytes.Buffer
	p := New(Config{Rate: 48000, Channels: 2, Sink: &out, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Volume: 0.5, Dither: 1})
	if _, err := p.pump(bytes.NewReader(make([]byte, 2*4000))); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(out.Bytes(), make([]byte, 2*4000)) {
		t.Error("silence came out as exact zeros although dither is on")
	}
}

func TestVolumeClampAndStatus(t *testing.T) {
	p := New(Config{Rate: 48000, Channels: 2, Sink: io.Discard, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if v := p.Status().Volume; v != 1 {
		t.Errorf("default volume %v, want 1", v)
	}
	if v := p.SetVolume(1.5); v != 1 {
		t.Errorf("SetVolume(1.5) = %v, want 1", v)
	}
	if v := p.SetVolume(-1); v != 0 {
		t.Errorf("SetVolume(-1) = %v, want 0", v)
	}
	if v := p.SetVolume(0.5); v != 0.5 || p.Volume() != 0.5 || p.Status().Volume != 0.5 {
		t.Errorf("SetVolume(0.5) = %v, Volume() = %v, Status().Volume = %v", v, p.Volume(), p.Status().Volume)
	}
	p2 := New(Config{Sink: io.Discard, Volume: 0.25})
	if p2.Volume() != 0.25 {
		t.Errorf("Config.Volume not applied: %v", p2.Volume())
	}
}
