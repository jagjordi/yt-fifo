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
