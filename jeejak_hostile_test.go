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
	"strings"
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
	fmt.Fprintf(&out, "%d 0 obj\n<< /Type /XRef /Size %d /Root 1 0 R /W [1 4 2] /Length %d >>\nstream\n",
		xrefNum, xrefNum+1, x.Len())
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
	case <-time.After(hostileDeadline):
		t.Fatalf("%d bytes: no answer within %s; the reader is still running", len(data), hostileDeadline)
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
// can refuse it.
func TestHostile_ContentThatInflatesPastTheCap(t *testing.T) {
	assertRefused(t, onePagePDF(bytes.Repeat([]byte("1 pop "), (maxInterpretBytes/6)+1024)), 512<<20)
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
