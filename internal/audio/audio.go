// Package audio streams a single track into a Discord voice connection.
// It shells out to ffmpeg to decode and Opus-encode whatever it's handed
// (e.g. piped from yt-dlp), reads the raw Opus packets back out of
// ffmpeg's Ogg container itself (see ogg.go), and buffers them up to
// frameBufferSize ahead of what's actually been sent, so a transient
// stall in ffmpeg or its upstream source doesn't immediately become an
// audible gap. Handle implements disgo/voice.OpusFrameProvider — the
// voice connection pulls frames from it at its own pace and handles
// pacing, speaking-state, and DAVE encryption itself.
package audio

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Opus frames are always 20ms in this pipeline (set via ffmpeg's
// -frame_duration below), matching Discord's expected packet cadence.
const frameDuration = 20 * time.Millisecond

// frameBufferSize is how many decoded Opus packets readLoop is allowed to
// get ahead of what's actually been pulled via ProvideOpusFrame — about 5
// seconds of audio. This is the slack that absorbs a transient stall
// upstream (a network hiccup in yt-dlp's live fetch, a slow encode tick)
// before it becomes an audible gap.
const frameBufferSize = 250

// zmqCommandTimeout bounds a single live volume change over the azmq
// control socket.
const zmqCommandTimeout = 3 * time.Second

var zmqSocketCounter atomic.Uint64

type Options struct {
	// Volume as a percentage, 100 = unchanged.
	Volume int
}

// Handle controls one in-flight track and implements voice.OpusFrameProvider.
// Callers get it back from Play, use Pause/Resume/Stop to control playback,
// and Done to learn when it ends (naturally, by error, or by Stop).
type Handle struct {
	cmd       *exec.Cmd
	ogg       *oggDemuxer
	stream    io.Closer
	zmqSocket string
	volumeMu  sync.Mutex // serializes SetVolume calls against one another

	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	stopCh chan struct{}
	doneCh chan error
	frames chan []byte // decoded packets, read ahead of ProvideOpusFrame

	readErrMu sync.Mutex
	readErr   error // set by readLoop on a non-EOF failure

	position        atomic.Int64 // nanoseconds of audio provided so far
	framesSent      atomic.Int64
	starvationCount atomic.Int64 // times ProvideOpusFrame had to wait for a frame

	mu       sync.Mutex
	paused   bool
	resumeCh chan struct{}
}

// Play starts encoding stream (already-decoded or containerized audio,
// e.g. piped from yt-dlp) into Opus via ffmpeg. stream is closed once
// playback ends, however it ends — the caller doesn't need a separate
// cleanup step. The returned Handle should be handed to a voice
// connection's SetOpusFrameProvider.
func Play(stream io.ReadCloser, opts Options) (*Handle, error) {
	vol := opts.Volume
	if vol <= 0 {
		vol = 100
	}

	sockPath := filepath.Join(os.TempDir(), fmt.Sprintf("noctune-azmq-%d-%d.sock", os.Getpid(), zmqSocketCounter.Add(1)))
	_ = os.Remove(sockPath)

	// azmq lets SetVolume adjust the volume filter live via internal/audio/zmq.go's
	// ZMTP client, without restarting the encoder. eval=frame makes the
	// volume filter re-evaluate its expression every frame, so a command
	// changing it takes effect on the next frame rather than needing a
	// restart. bind_address needs its colon backslash-escaped twice — once
	// for ffmpeg's filtergraph value parser, once for its AVOption string
	// parser underneath — confirmed empirically against a real azmq filter.
	afChain := fmt.Sprintf("azmq=bind_address=ipc\\\\://%s,volume=volume=%f:eval=frame", sockPath, float64(vol)/100)

	args := []string{
		// -re paces ffmpeg's reading/encoding to real time instead of
		// racing ahead as fast as the CPU allows. Without it, ffmpeg fills
		// the frames channel (frameBufferSize, ~5s) almost immediately and
		// stays pinned there — a live SetVolume only reaches audio ffmpeg
		// hasn't encoded *yet*, so up to 5 already-encoded seconds have to
		// play out first before it's audible. -re plus the 2s burst below
		// settles that lead to ~2s in steady state, so a volume change
		// lands in about that long instead of up to 5s.
		//
		// Bare -re would also remove the buffer's other job — absorbing a
		// stall (a yt-dlp network hiccup, or GuildPlayer.Pause leaving
		// ProvideOpusFrame uncalled) without an audible gap — since ffmpeg
		// would no longer run ahead to build any cushion. Paired with
		// -readrate_catchup, ffmpeg detects falling behind schedule and
		// reads at the catchup rate until it's caught up rather than
		// staying capped at real time; -readrate_initial_burst front-loads
		// a couple of seconds of cushion for whatever's left in flight the
		// instant a stall starts. Verified empirically (not just in
		// theory): piping a live-generated 15s Opus/WebM stream through
		// these exact flags with reads withheld for 6s (longer than
		// frameBufferSize) produced the full 15.0065s of output with zero
		// silence gaps and ffmpeg's own log confirming a clean catchup.
		//
		// Known residual tradeoff: catching up from a long stall/pause
		// runs at the catchup rate (3x) against 1x consumption, so a
		// pause of N seconds takes roughly N/2 more seconds after resume
		// with the buffer back near its 5s cap — i.e. SetVolume reverts to
		// the original ~5s lag for that recovery window, then settles back
		// to ~2s. Short pauses (a few seconds) recover fast enough not to
		// matter; a long AFK-style pause won't.
		"-re",
		"-readrate_initial_burst", "2",
		"-readrate_catchup", "3",
		"-thread_queue_size", "4096", "-i", "pipe:0",
		"-map", "0:a",
		"-acodec", "libopus",
		"-f", "ogg",
		"-page_duration", "20000",
		"-vbr", "on",
		"-compression_level", "10",
		"-ar", "48000",
		"-ac", "2",
		"-b:a", "128000",
		"-application", "audio",
		"-frame_duration", "20",
		"-loglevel", "warning",
		"-af", afChain,
		"pipe:1",
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdin = stream

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	h := &Handle{
		cmd:       cmd,
		ogg:       newOggDemuxer(stdout),
		stream:    stream,
		zmqSocket: sockPath,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan error, 1),
		frames:    make(chan []byte, frameBufferSize),
		resumeCh:  make(chan struct{}),
	}
	log.Printf("audio: encode session started")
	go h.readStderr(stderr)
	go h.readLoop()
	return h, nil
}

func (h *Handle) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		h.stderrMu.Lock()
		h.stderrBuf.WriteString(scanner.Text())
		h.stderrBuf.WriteByte('\n')
		h.stderrMu.Unlock()
	}
	if err := scanner.Err(); err != nil {
		h.stderrMu.Lock()
		fmt.Fprintf(&h.stderrBuf, "(stderr read error: %v)\n", err)
		h.stderrMu.Unlock()
	}
}

func (h *Handle) ffmpegMessages() string {
	h.stderrMu.Lock()
	defer h.stderrMu.Unlock()
	return h.stderrBuf.String()
}

// readLoop decodes Opus packets from ffmpeg's Ogg output as fast as
// ffmpeg produces them and queues them on h.frames, up to frameBufferSize
// ahead of what's actually been pulled via ProvideOpusFrame. It owns
// ffmpeg's process lifetime and reports the track's terminal state on
// doneCh once done.
func (h *Handle) readLoop() {
	defer func() {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
		_ = os.Remove(h.zmqSocket)
	}()
	defer func() {
		if err := h.stream.Close(); err != nil {
			log.Printf("audio: source stream close error: %v", err)
		}
	}()
	defer close(h.frames)

	// The first two packets of an Opus-in-Ogg stream are the OpusHead and
	// OpusTags metadata packets, not audio.
	skipPackets := 2

	// Log buffer level every ~10 seconds (500 frames × 20ms) to make
	// steady-state health visible in the logs.
	var logTick int

	for {
		frame, err := h.ogg.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if h.framesSent.Load() == 0 {
					log.Printf("audio: encode EOF with zero frames provided, ffmpeg likely failed to start: %s", h.ffmpegMessages())
				} else {
					log.Printf("audio: encode EOF after %d frames provided", h.framesSent.Load())
				}
				h.doneCh <- nil
			} else {
				h.readErrMu.Lock()
				h.readErr = fmt.Errorf("%w: %s", err, h.ffmpegMessages())
				h.readErrMu.Unlock()
				log.Printf("audio: ogg read error after %d frames provided: %v", h.framesSent.Load(), err)
				h.doneCh <- h.readErr
			}
			return
		}

		if skipPackets > 0 {
			skipPackets--
			continue
		}

		select {
		case h.frames <- frame:
		case <-h.stopCh:
			log.Printf("audio: stopped after %d frames provided", h.framesSent.Load())
			h.doneCh <- nil
			return
		}

		logTick++
		if logTick%500 == 0 {
			slog.Debug("audio: buffer level",
				"frames", len(h.frames),
				"max", frameBufferSize,
				"starvations", h.starvationCount.Load())
		}
	}
}

// ProvideOpusFrame implements voice.OpusFrameProvider. It's called by the
// voice connection's own paced sender, so it should never block for long:
// while paused it returns (nil, nil) immediately, which the sender takes
// as silence, rather than stalling the sender's pacing loop.
func (h *Handle) ProvideOpusFrame() ([]byte, error) {
	// Check stop first with priority — a racing select between stopCh and
	// a non-empty frames buffer picks randomly, producing choppy audio
	// until the buffer drains.
	select {
	case <-h.stopCh:
		return nil, io.EOF
	default:
	}

	h.mu.Lock()
	paused := h.paused
	h.mu.Unlock()
	if paused {
		return nil, nil
	}

	// Fast path: frame already in buffer.
	select {
	case frame, ok := <-h.frames:
		if !ok {
			h.readErrMu.Lock()
			err := h.readErr
			h.readErrMu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		h.position.Add(int64(frameDuration))
		if h.framesSent.Add(1) == 1 {
			log.Printf("audio: first opus frame provided to voice connection")
		}
		return frame, nil
	case <-h.stopCh:
		return nil, io.EOF
	default:
	}

	// Slow path: buffer empty — starvation event; block and log.
	n := h.starvationCount.Add(1)
	t0 := time.Now()
	slog.Debug("audio: buffer starvation", "count", n, "buf", len(h.frames), "max", frameBufferSize)
	select {
	case frame, ok := <-h.frames:
		if !ok {
			h.readErrMu.Lock()
			err := h.readErr
			h.readErrMu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		slog.Debug("audio: starvation recovered", "count", n, "after", time.Since(t0).Round(time.Millisecond))
		h.position.Add(int64(frameDuration))
		if h.framesSent.Add(1) == 1 {
			log.Printf("audio: first opus frame provided to voice connection")
		}
		return frame, nil
	case <-h.stopCh:
		return nil, io.EOF
	}
}

// Close implements voice.OpusFrameProvider. It's called by the voice
// connection when this provider is replaced or the connection closes.
func (h *Handle) Close() {
	h.Stop()
}

func (h *Handle) Pause() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paused = true
}

func (h *Handle) Resume() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.paused {
		h.paused = false
		close(h.resumeCh)
		h.resumeCh = make(chan struct{})
	}
}

// SetVolume changes the running ffmpeg encoder's volume filter live, over
// its azmq control socket, so a mid-track volume change doesn't restart
// the track.
func (h *Handle) SetVolume(pct int) error {
	h.volumeMu.Lock()
	defer h.volumeMu.Unlock()

	c, err := dialZMQReq(h.zmqSocket, zmqCommandTimeout)
	if err != nil {
		return fmt.Errorf("connect to azmq control socket: %w", err)
	}
	defer func() { _ = c.Close() }()

	reply, err := c.Send(fmt.Sprintf("all volume %f", float64(pct)/100))
	if err != nil {
		return fmt.Errorf("send volume command: %w", err)
	}
	// ffmpeg's azmq filter replies "<code> <message>", code 0 on success
	// (see libavfilter/f_zmq.c).
	var code int
	if _, err := fmt.Sscanf(reply, "%d", &code); err != nil || code != 0 {
		return fmt.Errorf("azmq: command failed: %s", reply)
	}
	return nil
}

// Stop ends playback. Safe to call more than once. Kills ffmpeg directly
// so a readLoop goroutine blocked reading a packet from a stalled process
// unblocks immediately instead of waiting for it to notice stopCh.
func (h *Handle) Stop() {
	select {
	case <-h.stopCh:
	default:
		close(h.stopCh)
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
}

// Done reports how the track ended: nil for a natural finish or an
// explicit Stop, non-nil for an ffmpeg/encode failure.
func (h *Handle) Done() <-chan error {
	return h.doneCh
}

func (h *Handle) Position() time.Duration {
	return time.Duration(h.position.Load())
}
