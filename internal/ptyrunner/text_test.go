package ptyrunner

import (
	"bytes"
	"testing"
)

func TestTitleParserConsume(t *testing.T) {
	parser := &TitleParser{}
	chunks := [][]byte{
		[]byte("\x1b]0;✳ Claude"),
		[]byte(" Code\x07ignored"),
		[]byte("\x1b]0;⠂ Claude Code\x07"),
	}

	var titles []string
	for _, chunk := range chunks {
		titles = append(titles, parser.Consume(chunk)...)
	}

	if len(titles) != 2 {
		t.Fatalf("expected 2 titles, got %d", len(titles))
	}
	if titles[0] != "✳ Claude Code" {
		t.Fatalf("unexpected first title %q", titles[0])
	}
	if titles[1] != "⠂ Claude Code" {
		t.Fatalf("unexpected second title %q", titles[1])
	}
}

func TestNormalizePrompt(t *testing.T) {
	got := NormalizePrompt("first line\n\nsecond line\r\n  third")
	want := "first line second line third"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestWriteBracketedPaste(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteBracketedPaste(&buf, "hello world"); err != nil {
		t.Fatalf("WriteBracketedPaste returned error: %v", err)
	}
	if got, want := buf.String(), BracketedPasteStart+"hello world"+BracketedPasteEnd; got != want {
		t.Fatalf("WriteBracketedPaste = %q, want %q", got, want)
	}
}

func TestCompactVisibleTextStripsANSIEscapes(t *testing.T) {
	got := CompactVisibleText([]byte("\x1b]0;Title\x07Yes, I trust this folder\x1b[31m!"))
	want := "yesitrustthisfolder"
	if got != want {
		t.Fatalf("CompactVisibleText = %q, want %q", got, want)
	}
}
