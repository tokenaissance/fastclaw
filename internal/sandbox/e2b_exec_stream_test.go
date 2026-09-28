package sandbox

// P1: the exec reader takes the Connect stream frame by frame instead of
// reading the whole body first.
//
// Before this, one command's output existed three times over — the 101 MB body,
// the decoded 76 MB string, and the copy `String()` made of it (2026-09-14, two
// OOMKilled pods). What these two cases pin is the property that replaced it:
// the same 70 MB stream comes back bounded without the process ever holding it.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// frameStreamReader serves `frames` copies of one pre-encoded frame, then a
// closing frame, then EOF — without ever holding the stream. `read` is how much
// the caller actually took, which is what a stopped-early read is measured by.
type frameStreamReader struct {
	frame  []byte // one complete [flags][len][payload] envelope
	end    []byte
	frames int

	served int
	off    int
	read   int64
}

func newFrameStreamReader(frame, end []byte, frames int) *frameStreamReader {
	return &frameStreamReader{frame: frame, end: end, frames: frames, off: len(frame)}
}

func (r *frameStreamReader) total() int64 {
	return int64(r.frames)*int64(len(r.frame)) + int64(len(r.end))
}

func (r *frameStreamReader) Read(p []byte) (int, error) {
	for {
		if r.off < len(r.frame) {
			n := copy(p, r.frame[r.off:])
			r.off += n
			r.read += int64(n)
			return n, nil
		}
		if r.served >= r.frames {
			if len(r.end) == 0 {
				return 0, io.EOF
			}
			r.frame, r.off, r.end = r.end, 0, nil
			continue
		}
		r.served++
		r.off = 0
	}
}

// frameStreamTransport hands each request a fresh generated body. The `du`
// probe the over-cap path issues gets a canned answer instead of 70 MB.
type frameStreamTransport struct {
	frame  []byte
	end    []byte
	frames int
	duBody string

	bodies []*frameStreamReader
}

func (t *frameStreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	body := t.body(string(raw))
	t.bodies = append(t.bodies, body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(body),
		Request:    req,
	}, nil
}

func (t *frameStreamTransport) body(command string) *frameStreamReader {
	if strings.Contains(command, "du -ak") && t.duBody != "" {
		frame := dataEnvelope(t.duBody)
		return newFrameStreamReader(frame, endEnvelope(), 1)
	}
	return newFrameStreamReader(t.frame, t.end, t.frames)
}

func dataEnvelope(stdout string) []byte {
	payload, _ := json.Marshal(map[string]any{"event": map[string]any{
		"start": map[string]any{"pid": 4242}}})
	return connectEnvelope(payload)
}

func stdoutEnvelope(stdout string) []byte {
	payload, _ := json.Marshal(map[string]any{"event": map[string]any{
		"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString([]byte(stdout))}}})
	return connectEnvelope(payload)
}

func endEnvelope() []byte {
	payload, _ := json.Marshal(map[string]any{"event": map[string]any{
		"end": map[string]any{"exited": true, "status": "exit status 0"}}})
	return connectEnvelope(payload)
}

// 70 MB of output, delivered in ~87 KB frames: the result must come back
// bounded, and the allocations must stay near the stream's own size rather than
// the two extra copies the read-everything version made.
func TestExecStreamsA70MBResultWithoutHoldingIt(t *testing.T) {
	const frames = 800
	inFrame := strings.Repeat("x", 64<<10)
	transport := &frameStreamTransport{
		frame:  stdoutEnvelope(inFrame),
		end:    endEnvelope(),
		frames: frames,
	}
	ex := testExecutor(&leaseCloseRecorder{}, "sb-1", "tok-1")
	ex.client = &http.Client{Transport: transport}

	bodyBytes := newFrameStreamReader(transport.frame, transport.end, frames).total()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	out, err := ex.Exec(context.Background(), "cat /workspace/run.log", 60*time.Second)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !strings.Contains(out, "of output omitted") {
		t.Fatalf("a %d MB result must come back clipped, got %d bytes", bodyBytes>>20, len(out))
	}
	if len(out) > OutputHeadCap+OutputTailCap+400 {
		t.Fatalf("result is %d bytes — the cap must bound it", len(out))
	}
	if got := transport.bodies[0].read; got != bodyBytes {
		t.Fatalf("read %d of %d body bytes: the tool path must consume the stream to size it", got, bodyBytes)
	}

	// The exact ratio is not the point; the order of magnitude is. Streaming
	// costs the base64 decode of what it reads (~1.75x for 64 KiB frames).
	// Holding the body as well costs a further body + decoded + copy (~5x), so
	// 3x separates the two shapes with room for allocator noise.
	delta := after.TotalAlloc - before.TotalAlloc
	t.Logf("streamed %d MB of body, allocated %d MB", bodyBytes>>20, delta>>20)
	if delta > uint64(3*bodyBytes) {
		t.Fatalf("the call allocated %d MB for a %d MB stream — it must not hold the whole body as well",
			delta>>20, bodyBytes>>20)
	}
}

// The machine-payload path must stop reading at the cap: draining a body that
// is going to be refused is the same peak, just later.
func TestPayloadStreamStopsReadingAtTheCap(t *testing.T) {
	const frames = 900
	inFrame := strings.Repeat("x", 64<<10)
	transport := &frameStreamTransport{
		frame:  stdoutEnvelope(inFrame),
		end:    endEnvelope(),
		frames: frames,
		duBody: "26624\t/workspace/kronos-wide-run.log\n",
	}
	ex := testExecutor(&leaseCloseRecorder{}, "sb-1", "tok-1")
	ex.client = &http.Client{Transport: transport}

	_, err := ex.SnapshotWorkspace(t.Context())
	if err == nil {
		t.Fatal("a snapshot past the cap must fail")
	}
	if !strings.Contains(err.Error(), humanBytes(snapshotBase64Cap)) {
		t.Fatalf("the error must name the cap: %s", snippet([]byte(err.Error()), 200))
	}
	// …and it must carry its CLASS, not just say it in prose: this is the sentinel the lifecycle
	// layer classifies on, and prose is not a classifier (row 84).
	if !errors.Is(err, errSnapshotOverCap) {
		t.Fatalf("the over-cap error lost its class: %s", snippet([]byte(err.Error()), 200))
	}

	first := transport.bodies[0]
	if first.read >= first.total() {
		t.Fatalf("read the whole %d MB body before refusing it", first.total()>>20)
	}
	// ~64 KiB of base64 per frame, so the cap lands well inside the stream.
	if first.served >= frames {
		t.Fatalf("served %d of %d frames — the read must stop at the cap", first.served, frames)
	}
	if kept := snapshotBase64Cap; int64(kept) > first.read {
		t.Fatalf("stopped after %d bytes, which is below the cap (%d) — that would be a bug elsewhere",
			first.read, kept)
	}
}

// The stream that dies mid-frame is not a transport error: envd says "no exit
// status" that way, and hydrate's retry depends on reading it as one.
func TestStreamCutMidFrameStaysATruncatedExec(t *testing.T) {
	envelope := stdoutEnvelope("partial\n")
	// The stream stops inside the frame — no end frame follows, because a real
	// cut is the connection ending, not a corrupt frame followed by a valid one.
	cut := envelope[:len(envelope)-4]
	ex := testExecutor(&leaseCloseRecorder{}, "sb-1", "tok-1")
	ex.client = &http.Client{Transport: &frameStreamTransport{
		frame:  cut,
		frames: 1,
	}}

	out, err := ex.Exec(context.Background(), "echo partial", 30*time.Second)
	if err == nil {
		t.Fatal("a stream without its exit trailer must fail")
	}
	var truncated *execStreamTruncatedError
	if !errors.As(err, &truncated) {
		t.Fatalf("error = %T (%v), want *execStreamTruncatedError", err, err)
	}
	if !sandboxUnusable(err) {
		t.Fatal("a mid-frame cut is the class a fresh sandbox produces once")
	}
	// The frame never completed, so there is nothing decoded to show — and the
	// message has to say so rather than pretending the command printed nothing.
	if !strings.Contains(err.Error(), "did not exit cleanly") {
		t.Fatalf("error must describe the truncation: %v", err)
	}
	if strings.Contains(out, "partial") || strings.Contains(out, "cGFydGlhbAo=") {
		t.Fatalf("a half-arrived frame must not be reported as output: %q", out)
	}
	if !strings.Contains(out, "no output") {
		t.Fatalf("the placeholder is what the caller should get here, got %q", out)
	}
}

// Frames larger than the reader's buffer still decode: the payload buffer grows
// to the frame, it is not assumed to be small.
func TestFrameLargerThanTheReadBuffer(t *testing.T) {
	big := strings.Repeat("y", 300<<10)
	ex := testExecutor(&leaseCloseRecorder{}, "sb-1", "tok-1")
	ex.client = &http.Client{Transport: &frameStreamTransport{
		frame:  stdoutEnvelope(big),
		end:    endEnvelope(),
		frames: 1,
	}}

	out, err := ex.Exec(context.Background(), "cat big", 30*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.HasPrefix(out, "yyy") || !strings.Contains(out, "of output omitted") {
		t.Fatalf("a 300 KB single frame must arrive clipped, got %d bytes", len(out))
	}
	if strings.Contains(out, "\x00") {
		t.Fatal("the payload buffer was not sized to the frame")
	}
	if !bytes.HasSuffix([]byte(out), []byte("yyy")) {
		t.Fatal("the frame's end was lost")
	}
}
