// Command yt-fifo streams YouTube audio as raw PCM for Snapcast.
//
//	yt-fifo [serve] [flags]   run the player and its HTTP API
//	yt-fifo control [flags]   Snapcast stream-plugin bridge (launched by snapserver)
//	yt-fifo version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jagjordi/yt-fifo/internal/api"
	"github.com/jagjordi/yt-fifo/internal/control"
	"github.com/jagjordi/yt-fifo/internal/player"
	"github.com/jagjordi/yt-fifo/internal/sink"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "control":
		err = runControl(args)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yt-fifo:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `yt-fifo %s — YouTube audio as raw PCM for Snapcast

Usage:
  yt-fifo [serve] [flags]   run the player and HTTP API (default)
  yt-fifo control [flags]   Snapcast stream-plugin bridge
  yt-fifo version

Run "yt-fifo serve -h" or "yt-fifo control -h" for the flags.
`, version)
}

// envOr reads the first set environment variable from names.
func envOr(def string, names ...string) string {
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && v != "" {
			return v
		}
	}
	return def
}

func envInt(def int, names ...string) int {
	v := envOr("", names...)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yt-fifo: ignoring non-numeric %s=%q\n", names[0], v)
		return def
	}
	return n
}

func envFloat(def float64, names ...string) float64 {
	v := envOr("", names...)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yt-fifo: ignoring non-numeric %s=%q\n", names[0], v)
		return def
	}
	return f
}

// newLogger logs to stderr without timestamps: both snapserver (process
// sources with log_stderr=true) and docker prefix their own.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
}

func serve(args []string) error {
	fs := flag.NewFlagSet("yt-fifo serve", flag.ExitOnError)
	listen := fs.String("listen", envOr(":"+envOr("8080", "PORT"), "YT_FIFO_LISTEN"), "HTTP API address (env YT_FIFO_LISTEN, or PORT)")
	output := fs.String("output", envOr("-", "YT_FIFO_OUTPUT", "AUDIO_FIFO"), "where PCM goes: - (stdout), tcp://host:port, or a FIFO path (env YT_FIFO_OUTPUT, or AUDIO_FIFO)")
	rate := fs.Int("rate", envInt(48000, "YT_FIFO_RATE", "AUDIO_RATE"), "sample rate in Hz (env YT_FIFO_RATE, or AUDIO_RATE)")
	channels := fs.Int("channels", envInt(2, "YT_FIFO_CHANNELS", "AUDIO_CHANNELS"), "channel count (env YT_FIFO_CHANNELS, or AUDIO_CHANNELS)")
	volume := fs.Float64("volume", envFloat(1, "YT_FIFO_VOLUME"), "initial output volume, 0..1, applied in software; changeable at runtime via POST /volume (env YT_FIFO_VOLUME)")
	maxVolume := fs.Float64("max-volume", envFloat(1, "YT_FIFO_MAX_VOLUME"), "fixed output scaling, 0..1, on top of the volume: what volume 1.0 amounts to (env YT_FIFO_MAX_VOLUME)")
	ffmpeg := fs.String("ffmpeg", envOr("ffmpeg", "FFMPEG"), "ffmpeg binary (env FFMPEG)")
	ytdlp := fs.String("yt-dlp", envOr("yt-dlp", "YT_DLP"), "yt-dlp binary (env YT_DLP)")
	ytdlpArgs := fs.String("yt-dlp-args", envOr("", "YT_DLP_ARGS"), "extra yt-dlp arguments, space separated (env YT_DLP_ARGS)")
	resolveTimeout := fs.Duration("resolve-timeout", 60*time.Second, "maximum time for one yt-dlp call")
	logLevel := fs.String("log-level", envOr("info", "YT_FIFO_LOG_LEVEL"), "debug, info, warn or error (env YT_FIFO_LOG_LEVEL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rate <= 0 || *channels <= 0 {
		return errors.New("rate and channels must be positive")
	}
	if *volume <= 0 || *volume > 1 {
		return errors.New("volume must be between 0 (exclusive) and 1")
	}
	if *maxVolume <= 0 || *maxVolume > 1 {
		return errors.New("max-volume must be between 0 (exclusive) and 1")
	}
	log := newLogger(*logLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out, err := sink.Open(ctx, *output, log)
	if err != nil {
		return err
	}
	defer out.Close()

	resolver := &player.YtDlpResolver{
		Binary:    *ytdlp,
		ExtraArgs: strings.Fields(*ytdlpArgs),
		Timeout:   *resolveTimeout,
		Log:       log,
	}
	p := player.New(player.Config{
		Rate:      *rate,
		Channels:  *channels,
		FFmpeg:    *ffmpeg,
		Resolver:  resolver,
		Sink:      out,
		Log:       log,
		Volume:    *volume,
		MaxVolume: *maxVolume,
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *listen, err)
	}
	srv := &http.Server{
		Handler:           api.New(p, log, version, *output),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()
	log.Info("yt-fifo started", "version", version, "listen", ln.Addr().String(), "output", *output,
		"format", fmt.Sprintf("s16le %d Hz %d ch", *rate, *channels), "volume", *volume, "max_volume", *maxVolume)

	p.Run(ctx) // returns when ctx is cancelled

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("yt-fifo stopped")
	return nil
}

func runControl(args []string) error {
	fs := flag.NewFlagSet("yt-fifo control", flag.ExitOnError)
	apiURL := fs.String("api", envOr("http://127.0.0.1:8080", "YT_FIFO_API"), "base URL of the yt-fifo HTTP API (env YT_FIFO_API)")
	logLevel := fs.String("log-level", envOr("info", "YT_FIFO_LOG_LEVEL"), "debug, info, warn or error")
	// snapserver always appends these; accept and ignore them.
	fs.String("stream", "", "stream id (set by snapserver)")
	fs.String("snapcast-host", "", "set by snapserver")
	fs.String("snapcast-port", "", "set by snapserver")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*logLevel)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	b := &control.Bridge{API: *apiURL, Log: log}
	return b.Run(ctx, os.Stdin, os.Stdout)
}
