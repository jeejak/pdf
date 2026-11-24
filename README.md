# PDF Reader

[![Built with WeBuild](https://raw.githubusercontent.com/webuild-community/badge/master/svg/WeBuild.svg)](https://webuild.community)

A simple Go library which enables reading PDF files. Forked from https://github.com/rsc/pdf

## Jeejak fork

This is Jeejak's fork of [ledongthuc/pdf](https://github.com/ledongthuc/pdf),
kept public. It is the parent at commit `5e46805` (2026-10-20) plus a few
bounds against hostile input, which the Jeejak label reader needs because it
parses PDFs uploaded by third parties. The module path is unchanged
(`github.com/ledongthuc/pdf`), so a program uses the fork with a `replace`
directive and no import changes:

```
replace github.com/ledongthuc/pdf => github.com/jeejak/pdf <version>
```

What changed, and why. Each guard below is a cherry-pick from the hardening
branch at [Cwooper/pdf `harden`](https://github.com/Cwooper/pdf/tree/harden)
(catalogued in issue 92 of the parent repository), applied with its original
author and message; each commit names its source commit, and says which
hunks were left out because the files or helpers they touch are not on this
base.

| Guard | Closes | Source commit (author) |
|---|---|---|
| /Extends chains capped at 32 hops, object stream nesting capped at 32 levels, plus bounds on the xref /Size, /Index, /W and predictor /Columns | an object stream that /Extends itself looped forever; a cycle through object streams overflowed the goroutine stack, a fatal error | `8274fa0` (gage-marshall) |
| An object stream's header (/N, /First, /Length, /Extends) is read without following references into other object streams | a stream stored inside itself, or whose /N lives inside itself, overflowed the stack; nested headers doubled the work per level | `3c1de8b` (Cwooper) |
| An object stream decodes at most 16 MiB | an object placed past inflated zeros was inflated on every resolve | `3d015d1` (Cwooper) |
| Interpret reads at most 64 MiB of decoded content | a few KB of Flate fed the lexer gigabytes | `0384e0b` (Cwooper) |
| Interpret holds at most 2^18 operands, counting the entries of the arrays and dicts on its stack | a content stream of operands and no operator grew memory without bound (117 KB reached a 6,470 MiB heap) | `4ed0098`, `9d3255d` (Cwooper) |

A refusal is reported the way this package already reports malformed
input: `NewReader` returns an error, and the accessors with no error return
(`NumPage`, `Page`, `Value.Key`, `Interpret`) panic. A caller reading
untrusted files must recover, as it already had to. Measured on the 37
label PDFs Jeejak's reader is tested against, no reading changed.

`jeejak_hostile_test.go` builds each hostile file in the test and asserts a
refusal within a deadline with no goroutine left behind. The tests carried
by the source commits are not here, since the test files they extend come
from earlier commits on that branch.

The rest of the `harden` branch is not carried. It changes what callers see
(`NumPage` counts the pages it lists rather than the declared /Count, and
each Reader gets a decode budget), which Jeejak's reader does not want
without its own review.

The license is unchanged (BSD 3-Clause, see LICENSE).

Features
  - Get plain text content (without format)
  - Get Content (including all font and formatting information)

## Install:

`go get -u github.com/ledongthuc/pdf`

## Examples:

 - Check in examples/ folder


## Read plain text

```golang
package main

import (
	"bytes"
	"fmt"

	"github.com/ledongthuc/pdf"
)

func main() {
	pdf.DebugOn = true

	f, r, err := pdf.Open("./pdf_test.pdf")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	b, err := r.GetPlainText()
	if err != nil {
		panic(err)
	}
	buf.ReadFrom(b)
	content := buf.String()
	fmt.Println(content)
}
```

## Read all text with styles from PDF

```golang
package main

import (
	"fmt"

	"github.com/ledongthuc/pdf"
)

func main() {
	f, r, err := pdf.Open("./pdf_test.pdf")
	if err != nil {
		panic(err)
	}
	defer f.Close()

	sentences, err := r.GetStyledTexts()
	if err != nil {
		panic(err)
	}

	// Print all sentences
	for _, sentence := range sentences {
		fmt.Printf("Font: %s, Font-size: %f, x: %f, y: %f, content: %s \n",
			sentence.Font,
			sentence.FontSize,
			sentence.X,
			sentence.Y,
			sentence.S)
	}
}
```


## Read text grouped by rows

```golang
package main

import (
	"fmt"
	"os"

	"github.com/ledongthuc/pdf"
)

func main() {
	content, err := readPdf(os.Args[1]) // Read local pdf file
	if err != nil {
		panic(err)
	}
	fmt.Println(content)
	return
}

func readPdf(path string) (string, error) {
	f, r, err := pdf.Open(path)
	defer func() {
		_ = f.Close()
	}()
	if err != nil {
		return "", err
	}
	totalPage := r.NumPage()

	for pageIndex := 1; pageIndex <= totalPage; pageIndex++ {
		p := r.Page(pageIndex)
		if p.V.IsNull() || p.V.Key("Contents").Kind() == pdf.Null {
			continue
		}

		rows, _ := p.GetTextByRow()
		for _, row := range rows {
		    println(">>>> row: ", row.Position)
		    for _, word := range row.Content {
		        fmt.Println(word.S)
		    }
		}
	}
	return "", nil
}
```

## Demo
![Run example](https://i.gyazo.com/01fbc539e9872593e0ff6bac7e954e6d.gif)
