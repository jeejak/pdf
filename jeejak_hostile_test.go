package pdf

// Hostile files for the guards this fork carries (see README.md, "Jeejak
// fork"). Every file is built in the test, so no binary blob is committed.
// Each test asserts that the file is REFUSED (the reader reports an error
// or panics, which every caller of this package must recover from), that
// the refusal arrives within a short deadline, and that no goroutine is
// left behind once it has.
//
// A stack overflow is a fatal error, not a panic, so for the object
// stream cycles a missing guard does not fail one test: it kills the test
// binary. That is still a red run, and it is the failure these guards
// exist to prevent.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostileDeadline is generous: every refusal below arrives in
// milliseconds, and the race detector slows them several times.
const hostileDeadline = 10 * time.Second

// stmEntry is one object of a file built by buildXrefStreamPDF. A plain
// object has a body; an object stored in an object stream names its
// container and its index there instead.
type stmEntry struct {
	body      string
	container int
	index     int
}

// buildXrefStreamPDF writes objects 1..n (object n+1 is the xref stream)
// with a cross-reference stream, so objects may live in object streams.
// Object 1 must be the catalog.
func buildXrefStreamPDF(objs []stmEntry) []byte {
	return buildXrefStreamPDFWith(objs, "")
}

// buildXrefStreamPDFWith is buildXrefStreamPDF with the xref stream's
// /Size and /W replaced by xrefHdr when it is not empty, to put a hostile
// value in the cross-reference stream itself.
func buildXrefStreamPDFWith(objs []stmEntry, xrefHdr string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.5\n")
	offs := make([]int, len(objs)+1)
	for i, o := range objs {
		if o.container != 0 {
			continue
		}
		offs[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o.body)
	}
	xrefNum := len(objs) + 1
	var x bytes.Buffer
	ent := func(typ byte, a uint32, b uint16) {
		x.WriteByte(typ)
		binary.Write(&x, binary.BigEndian, a)
		binary.Write(&x, binary.BigEndian, b)
	}
	ent(0, 0, 65535)
	for i, o := range objs {
		if o.container != 0 {
			ent(2, uint32(o.container), uint16(o.index))
		} else {
			ent(1, uint32(offs[i+1]), 0)
		}
	}
	xrefOff := out.Len()
	ent(1, uint32(xrefOff), 0)
	if xrefHdr == "" {
		xrefHdr = fmt.Sprintf("/Size %d /W [1 4 2]", xrefNum+1)
	}
	fmt.Fprintf(&out, "%d 0 obj\n<< /Type /XRef %s /Root 1 0 R /Length %d >>\nstream\n",
		xrefNum, xrefHdr, x.Len())
	out.Write(x.Bytes())
	fmt.Fprintf(&out, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xrefOff)
	return out.Bytes()
}

// objStm is the body of an object stream holding data, with header
// entries hdr (which must include /N and /First).
func objStm(hdr, data string) string {
	return fmt.Sprintf("<< /Type /ObjStm %s /Length %d >>\nstream\n%s\nendstream", hdr, len(data), data)
}

// flateStream is the body of a Flate-compressed stream of content.
func flateStream(hdr string, content []byte) string {
	var z bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&z, zlib.BestCompression)
	zw.Write(content)
	zw.Close()
	return fmt.Sprintf("<< /Filter /FlateDecode%s /Length %d >>\nstream\n%s\nendstream", hdr, z.Len(), z.String())
}

// onePagePDF is a classic-xref file with one page drawing content.
func onePagePDF(content []byte) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents 4 0 R >>",
		flateStream("", content),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// readEverything opens data and reads what a text extractor reads: the
// page count, then each page's content through Interpret.
func readEverything(data []byte) error {
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	n := r.NumPage()
	for i := 1; i <= n && i <= 4; i++ {
		Interpret(r.Page(i).V.Key("Contents"), func(stk *Stack, op string) {
			for stk.Len() > 0 {
				stk.Pop()
			}
		})
	}
	return nil
}

// assertRefused runs read over data on its own goroutine and asserts a
// refusal (an error or a recovered panic) inside hostileDeadline, with
// the goroutine gone afterwards and the heap growth under maxAlloc.
func assertRefused(t *testing.T, data []byte, maxAlloc uint64) {
	t.Helper()
	assertRefusedWithin(t, data, maxAlloc, hostileDeadline)
}

// assertRefusedWithin is assertRefused under a chosen deadline, for the
// one file whose refusal means lexing the whole cap first.
func assertRefusedWithin(t *testing.T, data []byte, maxAlloc uint64, limit time.Duration) {
	t.Helper()
	runtime.GC()
	before := runtime.NumGoroutine()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	type outcome struct {
		err       error
		recovered interface{}
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- outcome{recovered: rec}
			}
		}()
		done <- outcome{err: readEverything(data)}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(limit):
		t.Fatalf("%d bytes: no answer within %s; the reader is still running", len(data), limit)
	}
	took := time.Since(start)
	if got.err == nil && got.recovered == nil {
		t.Fatalf("%d bytes: read to the end with no error and no panic; want a refusal", len(data))
	}

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	if grew := m1.TotalAlloc - m0.TotalAlloc; grew > maxAlloc {
		t.Errorf("%d bytes: allocated %d MiB before refusing, want under %d MiB", len(data), grew>>20, maxAlloc>>20)
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if left := runtime.NumGoroutine() - before; left > 0 {
		t.Errorf("%d goroutines still running after the refusal", left)
	}
	t.Logf("%d bytes refused in %s (err=%v, panic=%v)", len(data), took.Round(time.Millisecond), got.err != nil, got.recovered != nil)
}

const smallAlloc = 64 << 20

// TestHostile_ObjectStreamStoredInsideItself: the cross-reference stream
// records object stream 5 as stored inside object stream 5, and the
// /Pages object lives in it. Resolving /Pages resolved its container,
// which resolved its container, until the goroutine stack overflowed
// (jeejak/paket.jeejak.id#591, the 312-byte file).
func TestHostile_ObjectStreamStoredInsideItself(t *testing.T) {
	data := buildXrefStreamPDF([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{container: 5, index: 0},
		{body: "null"},
		{body: "null"},
		{container: 5, index: 0},
	})
	assertRefused(t, data, smallAlloc)
}

// TestHostile_ObjectStreamWhoseNIsInsideItself: object stream 5's /N is
// a reference to object 6, which the cross-reference stream places in
// stream 5. Reading /N to find object 6 resolves object 6, which reads
// /N again (jeejak/paket.jeejak.id#591, the 321-byte file).
func TestHostile_ObjectStreamWhoseNIsInsideItself(t *testing.T) {
	data := buildXrefStreamPDF([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{container: 5, index: 0},
		{body: "null"},
		{body: "null"},
		{body: objStm("/N 6 0 R /First 12", "2 0 6 2 1 1")},
		{container: 5, index: 1},
	})
	assertRefused(t, data, smallAlloc)
}

// TestHostile_ObjectStreamThatExtendsItself: the /Pages object is
// missing from object stream 5, whose /Extends names stream 5 again, so
// the search for it never ended (jeejak/paket.jeejak.id#588).
func TestHostile_ObjectStreamThatExtendsItself(t *testing.T) {
	data := buildXrefStreamPDF([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{container: 5, index: 0},
		{body: "null"},
		{body: "null"},
		{body: objStm("/N 1 /First 4 /Extends 5 0 R", "9 0 << /A 1 >>")},
	})
	assertRefused(t, data, smallAlloc)
}

// TestHostile_ObjectStreamHeadersNestedIntoOtherStreams: a chain of
// object streams, each of whose /N and /First live in the next. Without
// the header view every level decoded the next stream once per header
// entry, so the work doubled per level; 20 levels is about a million
// decodes. Refused at the first level instead.
func TestHostile_ObjectStreamHeadersNestedIntoOtherStreams(t *testing.T) {
	const levels = 20
	// Objects: 1 catalog, 2 /Pages (in stream 3), 3..3+levels-1 the
	// streams, then the two header integers of each stream, stored in
	// the next stream (the last stream's are plain objects).
	stream := func(k int) int { return 3 + k }
	nRef := func(k int) int { return 3 + levels + 2*k }
	fRef := func(k int) int { return nRef(k) + 1 }

	// Each stream's contents, as (object number, object) pairs: stream 0
	// holds /Pages, stream k holds stream k-1's two header integers.
	type held struct {
		num  int
		body string
	}
	headers := make([][2]int, levels) // /N and /First per stream
	data := make([]string, levels)
	contents := make([][]held, levels)
	contents[0] = []held{{2, "<< /Type /Pages /Kids [] /Count 0 >>"}}
	for k := 0; k < levels; k++ {
		if k > 0 {
			contents[k] = []held{
				{nRef(k - 1), fmt.Sprint(headers[k-1][0])},
				{fRef(k - 1), fmt.Sprint(headers[k-1][1])},
			}
		}
		var index, objsText strings.Builder
		for _, h := range contents[k] {
			fmt.Fprintf(&index, "%d %d ", h.num, objsText.Len())
			objsText.WriteString(h.body + " ")
		}
		headers[k] = [2]int{len(contents[k]), index.Len()}
		data[k] = index.String() + objsText.String()
	}

	objs := make([]stmEntry, 3+levels+2*levels-1)
	objs[0] = stmEntry{body: "<< /Type /Catalog /Pages 2 0 R >>"}
	objs[1] = stmEntry{container: stream(0), index: 0}
	for k := 0; k < levels; k++ {
		objs[stream(k)-1] = stmEntry{body: objStm(
			fmt.Sprintf("/N %d 0 R /First %d 0 R", nRef(k), fRef(k)), data[k])}
		if k == levels-1 {
			objs[nRef(k)-1] = stmEntry{body: fmt.Sprint(headers[k][0])}
			objs[fRef(k)-1] = stmEntry{body: fmt.Sprint(headers[k][1])}
		} else {
			objs[nRef(k)-1] = stmEntry{container: stream(k + 1), index: 0}
			objs[fRef(k)-1] = stmEntry{container: stream(k + 1), index: 1}
		}
	}
	assertRefused(t, buildXrefStreamPDF(objs), smallAlloc)
}

// TestHostile_ObjectPastAnInflatedGap: an object stream whose index
// places an object 32 MiB into it, past Flate-compressed zeros, so every
// resolve inflated the whole gap and held it in the lexer.
func TestHostile_ObjectPastAnInflatedGap(t *testing.T) {
	const gap = 32 << 20
	index := fmt.Sprintf("2 %d ", gap)
	first := len(index)
	content := append([]byte(index), bytes.Repeat([]byte(" "), gap)...)
	content = append(content, []byte("<< /Type /Pages /Kids [] /Count 0 >>")...)
	data := buildXrefStreamPDF([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{container: 3, index: 0},
		{body: flateStream(fmt.Sprintf(" /Type /ObjStm /N 1 /First %d", first), content)},
	})
	assertRefused(t, data, 256<<20)
}

// TestHostile_OperandsWithNoOperator: a content stream of numbers and
// no operator. Interpret calls back only on an operator, so every number
// stayed on the stack: 117 KB of Flate reached a 6,470 MiB heap
// (jeejak/paket.jeejak.id#586).
func TestHostile_OperandsWithNoOperator(t *testing.T) {
	assertRefused(t, onePagePDF(bytes.Repeat([]byte("1 "), 2_000_000)), 256<<20)
}

// TestHostile_OneHugeArrayOperand: the same memory in a single operand,
// an array of millions of numbers, which a cap on the stack alone does
// not see.
func TestHostile_OneHugeArrayOperand(t *testing.T) {
	content := "[" + strings.Repeat("0 ", 2_000_000) + "] TJ"
	assertRefused(t, onePagePDF([]byte(content)), 256<<20)
}

// TestHostile_ContentThatInflatesPastTheCap: more decoded content than
// Interpret reads, in operators that hold nothing, so only the byte cap
// can refuse it. The refusal comes after lexing the 16 MiB cap, about
// 0.36 s, and under the race detector 4.5 s (measured 12.6 times
// slower on this content), hence the minute.
// Its operands hold nothing, so the allocation bound here is only a
// sanity check: what this test pins is the refusal itself.
func TestHostile_ContentThatInflatesPastTheCap(t *testing.T) {
	assertRefusedWithin(t, onePagePDF(bytes.Repeat([]byte("1 pop "), (maxInterpretBytes/6)+1024)), 2<<30, time.Minute)
}

// TestHostile_ALegitimateObjectStreamStillReads keeps the guards honest:
// an object stream holding the page tree, with direct header values and
// an /Extends to a second stream, reads its one page.
func TestHostile_ALegitimateObjectStreamStillReads(t *testing.T) {
	// 1 catalog, 2 /Pages (in stream 6), 3 the page (recorded in
	// stream 6 but found in stream 5, which stream 6 extends), 4 the
	// content, 5 and 6 the streams.
	pages := "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"
	page := "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents 4 0 R >>"
	data := buildXrefStreamPDF([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{container: 6, index: 0},
		{container: 6, index: 0},
		{body: flateStream("", []byte("BT (ok) Tj ET"))},
		{body: objStm("/N 1 /First 4", "3 0 "+page)},
		{body: objStm("/N 1 /First 4 /Extends 5 0 R", "2 0 "+pages)},
	})
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if n := r.NumPage(); n != 1 {
		t.Fatalf("NumPage = %d, want 1", n)
	}
	var ops []string
	Interpret(r.Page(1).V.Key("Contents"), func(stk *Stack, op string) {
		ops = append(ops, op)
		for stk.Len() > 0 {
			stk.Pop()
		}
	})
	if got := strings.Join(ops, " "); got != "BT Tj ET" {
		t.Fatalf("operators = %q, want %q", got, "BT Tj ET")
	}
}

// peakHeapDuring runs fn and reports the largest live heap seen while it
// ran, sampled every millisecond. TotalAlloc cannot tell a reader that
// opens a decoder at a time from one that holds them all, since both
// allocate the same in sum; the peak can.
func peakHeapDuring(fn func()) uint64 {
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
		for {
			metrics.Read(sample)
			if v := sample[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	runtime.GC()
	fn()
	close(stop)
	<-sampled
	return peak.Load()
}

// contentsArrayPDF is one page whose /Contents array names one small
// Flate stream k times, with each stream decoded through filters stages
// of /FlateDecode.
func contentsArrayPDF(k, filters int) []byte {
	return contentsArrayPDFOf([]byte("BT ET\n"), k, filters)
}

// contentsArrayPDFOf is contentsArrayPDF with the stream's decoded
// content given.
func contentsArrayPDFOf(content []byte, k, filters int) []byte {
	for i := 0; i < filters; i++ {
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(content)
		zw.Close()
		content = z.Bytes()
	}
	filter := "/FlateDecode"
	if filters > 1 {
		filter = "[" + strings.TrimSpace(strings.Repeat("/FlateDecode ", filters)) + "]"
	}
	refs := strings.Repeat("4 0 R ", k)
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents [" + refs + "] >>",
		fmt.Sprintf("<< /Filter %s /Length %d >>\nstream\n%s\nendstream", filter, len(content), content),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// TestHostile_AContentsArrayOpensOneDecoderAtATime: a page whose
// /Contents array names one Flate stream maxContentsStreams times, the
// most a page may. Interpret opened a decoder for every entry before
// reading any, about 45 KiB each (20,000 entries peaked at 893 MiB). It
// now opens each when the one before it ends, and the page still reads.
func TestHostile_AContentsArrayOpensOneDecoderAtATime(t *testing.T) {
	data := contentsArrayPDF(maxContentsStreams, 1)
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	ops := 0
	start := time.Now()
	peak := peakHeapDuring(func() {
		Interpret(r.Page(1).V.Key("Contents"), func(stk *Stack, op string) { ops++ })
	})
	if want := 2 * maxContentsStreams; ops != want {
		t.Fatalf("read %d operators, want %d", ops, want)
	}
	if peak > 16<<20 {
		t.Fatalf("%d bytes: the heap peaked at %d MiB while reading, want under 16 MiB: "+
			"the decoders are opened up front again", len(data), peak>>20)
	}
	t.Logf("%d bytes: %d operators in %s, heap peak %d MiB", len(data), ops,
		time.Since(start).Round(time.Millisecond), peak>>20)
}

// TestHostile_ALongContentsArrayIsRefusedQuickly: one 8-stage Flate
// stream named 262,000 times (about 1.5 MB). Read one stream at a time
// the heap stays small, but every entry builds eight decoders, so one
// Interpret call ran about 11 seconds, past any caller's deadline and out
// of reach of its stop flag. An array over maxContentsStreams is refused
// before any stream is opened.
func TestHostile_ALongContentsArrayIsRefusedQuickly(t *testing.T) {
	data := contentsArrayPDF(262_000, 8)
	start := time.Now()
	assertRefused(t, data, 256<<20)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("refused after %s, want under 2s", took)
	}
}

// TestHostile_TheMostContentStreamsAPageMayHaveReadInBoundedTime is the
// cap's own time bound: maxContentsStreams entries of an 8-stage stream,
// the most the two caps allow together.
func TestHostile_TheMostContentStreamsAPageMayHaveReadInBoundedTime(t *testing.T) {
	data := contentsArrayPDF(maxContentsStreams, maxFilters)
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	start := time.Now()
	ops := 0
	Interpret(r.Page(1).V.Key("Contents"), func(stk *Stack, op string) { ops++ })
	took := time.Since(start)
	if want := 2 * maxContentsStreams; ops != want {
		t.Fatalf("read %d operators, want %d", ops, want)
	}
	if took > 2*time.Second {
		t.Fatalf("one Interpret call took %s, want under 2s", took)
	}
	t.Logf("%d streams of %d stages: %s", maxContentsStreams, maxFilters, took.Round(time.Millisecond))
}

// streamsPDF is one page whose /Contents array is the given streams, in
// order, uncompressed.
func streamsPDF(streams ...string) []byte {
	var refs strings.Builder
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"",
	}
	for i, c := range streams {
		fmt.Fprintf(&refs, "%d 0 R ", 4+i)
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(c), c))
	}
	objs[2] = "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents [" + refs.String() + "] >>"
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs))
	for i, o := range objs {
		offs[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

// TestContents_StreamsJoinWithNoSeparator pins the join upstream chose
// (ledongthuc/pdf 8b43568, "Fix tokens split across content streams"): a
// /Contents array reads as its streams' bytes back to back, so a string
// or an operator split across two streams reads as one token. The harden
// branch puts a newline between streams instead; carrying that would
// change what such a page reads, and this test would fail.
func TestContents_StreamsJoinWithNoSeparator(t *testing.T) {
	data := streamsPDF("BT (Hel", "lo) T", "j ET")
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var ops, strs []string
	Interpret(r.Page(1).V.Key("Contents"), func(stk *Stack, op string) {
		ops = append(ops, op)
		if op == "Tj" {
			strs = append(strs, stk.Pop().RawString())
		}
		for stk.Len() > 0 {
			stk.Pop()
		}
	})
	if got := strings.Join(ops, " "); got != "BT Tj ET" {
		t.Fatalf("operators = %q, want %q", got, "BT Tj ET")
	}
	if len(strs) != 1 || strs[0] != "Hello" {
		t.Fatalf("Tj strings = %q, want [\"Hello\"]", strs)
	}
}

// TestHostile_TooManyStackedFiltersAreRefused: one stream decoded
// through twenty /FlateDecode stages, named 400 times by a /Contents
// array. Every stage's decoder is built before a byte is read.
func TestHostile_TooManyStackedFiltersAreRefused(t *testing.T) {
	assertRefused(t, contentsArrayPDF(400, 20), smallAlloc)
}

// TestHostile_EightStackedFiltersStillRead pins the filter cap from
// below: eight stages is the most a stream may chain, and it reads.
func TestHostile_EightStackedFiltersStillRead(t *testing.T) {
	data := contentsArrayPDF(1, 8)
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var ops []string
	Interpret(r.Page(1).V.Key("Contents"), func(stk *Stack, op string) { ops = append(ops, op) })
	if got := strings.Join(ops, " "); got != "BT ET" {
		t.Fatalf("operators = %q, want %q", got, "BT ET")
	}
}

// The cross-reference stream bounds from 8274fa0. Each value below is
// read while the file is opened, so NewReader must return an error for
// it, quickly and without allocating what the value asks for.
func xrefHostileFile(xrefHdr string) []byte {
	return buildXrefStreamPDFWith([]stmEntry{
		{body: "<< /Type /Catalog /Pages 2 0 R >>"},
		{body: "<< /Type /Pages /Kids [] /Count 0 >>"},
	}, xrefHdr)
}

// TestHostile_AnXrefStreamSizeOfTwoToTheThirtyIsRefused: /Size sized
// the table with make directly, so 2^30 asked for about 40 GiB.
func TestHostile_AnXrefStreamSizeOfTwoToTheThirtyIsRefused(t *testing.T) {
	assertRefused(t, xrefHostileFile(fmt.Sprintf("/Size %d /W [1 4 2]", 1<<30)), smallAlloc)
}

// TestHostile_AnXrefStreamIndexFarOutIsRefused: /Index names object
// numbers that index the table, so [4000000000 1] grew it by about
// 128 GB for one entry.
func TestHostile_AnXrefStreamIndexFarOutIsRefused(t *testing.T) {
	assertRefused(t, xrefHostileFile("/Size 4 /Index [4000000000 1] /W [1 4 2]"), smallAlloc)
}

// TestHostile_AnXrefStreamFieldWidthOverEightIsRefused: a /W width is
// a slice bound and sizes the read buffer. 99 was already refused
// upstream, since the row it implies runs past the stream; 2^30 was not,
// and sized a gigabyte buffer for one row. Anything over eight bytes is
// now refused before the buffer is made.
func TestHostile_AnXrefStreamFieldWidthOverEightIsRefused(t *testing.T) {
	for _, w := range []int{99, 1 << 30} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			assertRefused(t, xrefHostileFile(fmt.Sprintf("/Size 4 /W [1 %d 2]", w)), smallAlloc)
		})
	}
}

// nestedArraysContent is n repetitions of an array nested depth levels
// deep followed by `pop`, which Interpret runs itself: the most lexing
// work per decoded byte that no callback ever sees.
func nestedArraysContent(depth, n int) []byte {
	unit := strings.Repeat("[", depth) + strings.Repeat("]", depth) + " pop "
	return bytes.Repeat([]byte(unit), n)
}

// TestHostile_NestedArraysInflatingPastTheCapAreRefusedInBoundedTime: a
// 1.5 KB page whose one 8-stage Flate stream decodes to 64 MiB of arrays
// nested 999 deep, each followed by `pop`. No operator reaches the
// callback, so the whole stream is one stretch no stop flag can reach. At
// harden's 64 MiB cap that was about 4 s (3.9 s measured); at 16 MiB it is
// refused after about 1 s.
func TestHostile_NestedArraysInflatingPastTheCapAreRefusedInBoundedTime(t *testing.T) {
	data := contentsArrayPDFOf(nestedArraysContent(999, (64<<20)/2003+64), 1, 8)
	start := time.Now()
	assertRefusedWithin(t, data, 2<<30, time.Minute)
	took := time.Since(start)
	// 2.5 s is the ceiling without the race detector. Under it this
	// content measured 3.8 to 4.1 times slower (1.4 s against 5.4 s),
	// so the ceiling is skipped there and only the refusal is asserted.
	if !raceEnabled && took > 2500*time.Millisecond {
		t.Fatalf("%d bytes refused after %s, want under 2.5s", len(data), took)
	}
	t.Logf("%d bytes refused after %s", len(data), took.Round(time.Millisecond))
}

// TestHostile_TheMostContentStreamsOfRealContentAreRefusedInBoundedTime:
// 1,024 entries, the most a /Contents array may have, of an 8-stage
// stream decoding to 64 KiB of the same nested arrays. The array cap does
// not bound this; the Interpret byte cap does, at 16 MiB.
func TestHostile_TheMostContentStreamsOfRealContentAreRefusedInBoundedTime(t *testing.T) {
	data := contentsArrayPDFOf(nestedArraysContent(999, 32), maxContentsStreams, 8)
	start := time.Now()
	assertRefusedWithin(t, data, 2<<30, time.Minute)
	took := time.Since(start)
	if !raceEnabled && took > 2500*time.Millisecond {
		t.Fatalf("%d bytes refused after %s, want under 2.5s", len(data), took)
	}
	t.Logf("%d bytes refused after %s", len(data), took.Round(time.Millisecond))
}
