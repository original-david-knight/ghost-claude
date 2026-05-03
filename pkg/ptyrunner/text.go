package ptyrunner

import (
	"context"
	"io"
	"strings"
	"time"
)

const (
	BracketedPasteStart = "\x1b[200~"
	BracketedPasteEnd   = "\x1b[201~"
	titleParserMaxBytes = 1024
)

type TitleParser struct {
	pending string
}

func (p *TitleParser) Consume(chunk []byte) []string {
	p.pending += string(chunk)

	var titles []string
	for {
		start := strings.Index(p.pending, "\x1b]0;")
		if start == -1 {
			p.trim()
			return titles
		}

		if start > 0 {
			p.pending = p.pending[start:]
		}

		body := p.pending[len("\x1b]0;"):]
		belIndex := strings.Index(body, "\x07")
		stIndex := strings.Index(body, "\x1b\\")

		endIndex := -1
		terminatorLength := 0
		switch {
		case belIndex >= 0 && (stIndex == -1 || belIndex < stIndex):
			endIndex = belIndex
			terminatorLength = 1
		case stIndex >= 0:
			endIndex = stIndex
			terminatorLength = 2
		default:
			p.trim()
			return titles
		}

		titles = append(titles, body[:endIndex])
		p.pending = body[endIndex+terminatorLength:]
	}
}

func (p *TitleParser) trim() {
	if len(p.pending) > titleParserMaxBytes {
		p.pending = p.pending[len(p.pending)-titleParserMaxBytes:]
	}
}

func NormalizePrompt(prompt string) string {
	replaced := strings.ReplaceAll(prompt, "\r\n", "\n")
	replaced = strings.ReplaceAll(replaced, "\r", "\n")
	return strings.Join(strings.Fields(replaced), " ")
}

func WriteBracketedPaste(w io.Writer, payload string) error {
	if _, err := io.WriteString(w, BracketedPasteStart); err != nil {
		return err
	}
	if _, err := io.WriteString(w, payload); err != nil {
		return err
	}
	if _, err := io.WriteString(w, BracketedPasteEnd); err != nil {
		return err
	}
	return nil
}

func Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func CompactVisibleText(chunk []byte) string {
	var out strings.Builder

	for i := 0; i < len(chunk); {
		if chunk[i] == 0x1b {
			i++
			if i >= len(chunk) {
				break
			}

			switch chunk[i] {
			case '[':
				i++
				for i < len(chunk) && ((chunk[i] >= 0x30 && chunk[i] <= 0x3f) || (chunk[i] >= 0x20 && chunk[i] <= 0x2f)) {
					i++
				}
				if i < len(chunk) {
					i++
				}
			case ']':
				i++
				for i < len(chunk) {
					if chunk[i] == 0x07 {
						i++
						break
					}
					if chunk[i] == 0x1b && i+1 < len(chunk) && chunk[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			default:
				i++
			}
			continue
		}

		r := rune(chunk[i])
		i++

		if r >= 'A' && r <= 'Z' {
			out.WriteRune(r + ('a' - 'A'))
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		}
	}

	return out.String()
}
