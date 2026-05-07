package diagnostics

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	mediaText        = "text/plain; charset=utf-8"
	mediaJSON        = "application/json"
	mediaOctetStream = "application/octet-stream"
)

type Options struct {
	Workspace string
	DebugRoot string
	Now       func() time.Time
}

type Capturer struct {
	debugRoot string
	now       func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func New(workspace string) *Capturer {
	return NewWithOptions(Options{Workspace: workspace})
}

func NewWithOptions(opts Options) *Capturer {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		workspace = "."
	}

	debugRoot := strings.TrimSpace(opts.DebugRoot)
	if debugRoot == "" {
		debugRoot = filepath.Join(workspace, ".vibedrive", "debug")
	} else if !filepath.IsAbs(debugRoot) {
		debugRoot = filepath.Join(workspace, debugRoot)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	return &Capturer{
		debugRoot: filepath.Clean(debugRoot),
		now:       now,
		locks:     make(map[string]*sync.Mutex),
	}
}

func (c *Capturer) DebugRoot() string {
	if c == nil {
		return filepath.Join(".", ".vibedrive", "debug")
	}
	return c.debugRoot
}

func (c *Capturer) StepDir(id Identity) (string, error) {
	if c == nil {
		c = New(".")
	}
	segments, err := derivePathSegments(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.debugRoot, segments.run, segments.task, segments.step), nil
}

func (c *Capturer) CaptureExec(req ExecCapture) (Result, error) {
	if c == nil {
		c = New(".")
	}
	req.Transport = defaultExecTransport(req.Transport)
	return c.captureExec(req)
}

func (c *Capturer) CaptureTmux(req TmuxCapture) (Result, error) {
	if c == nil {
		c = New(".")
	}
	req.Transport = defaultTmuxTransport(req.Transport)
	return c.captureTmux(req)
}

func (c *Capturer) captureExec(req ExecCapture) (Result, error) {
	stepDir, segments, manifest, err := c.startManifest(req.Identity, req.Failure, req.Transport)
	if err != nil {
		return Result{}, err
	}

	unlock := c.lockStep(stepDir)
	defer unlock()

	if err := os.MkdirAll(stepDir, 0o755); err != nil {
		return Result{}, err
	}

	var artifactErrs []error
	add := func(entry ArtifactEntry, err error) {
		manifest.Artifacts = append(manifest.Artifacts, entry)
		if err != nil {
			artifactErrs = append(artifactErrs, err)
		}
	}

	add(c.writeByteArtifact(stepDir, ArtifactParentStdout, "parent/stdout-tail.txt", mediaText, true, req.ParentStdout, ParentOutputLimit, boundTailBytes))
	add(c.writeByteArtifact(stepDir, ArtifactParentStderr, "parent/stderr-tail.txt", mediaText, true, req.ParentStderr, ParentOutputLimit, boundTailBytes))

	if req.Prompt != nil {
		promptEntries, promptErrs := c.writePromptArtifacts(stepDir, *req.Prompt, true)
		for _, entry := range promptEntries {
			manifest.Artifacts = append(manifest.Artifacts, entry)
		}
		artifactErrs = append(artifactErrs, promptErrs...)
	} else {
		manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactPromptRaw, "prompt/raw.bin", mediaOctetStream))
		manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactPromptNormal, "prompt/normalized.txt", mediaText))
		manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactPromptMetadata, "prompt/metadata.json", mediaJSON))
	}

	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactTmuxPane, "tmux/pane.txt", mediaText))
	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactTmuxTitles, "tmux/title-history.jsonl", "application/x-ndjson"))
	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactTmuxMetadata, "tmux/metadata.json", mediaJSON))

	add(c.writeJSONArtifact(stepDir, ArtifactExecCommand, "exec/command.json", true, sanitizeExecCommand(req.Command), compactExecCommand))
	add(c.writeByteArtifact(stepDir, ArtifactExecStdout, "exec/stdout-tail.txt", mediaText, true, req.Stdout, ExecOutputLimit, boundTailBytes))
	add(c.writeByteArtifact(stepDir, ArtifactExecStderr, "exec/stderr-tail.txt", mediaText, true, req.Stderr, ExecOutputLimit, boundTailBytes))
	add(c.writeByteArtifact(stepDir, ArtifactExecCombined, "exec/combined-tail.txt", mediaText, true, req.Combined, ExecOutputLimit, boundTailBytes))

	result, manifestErr := c.finishManifest(stepDir, segments, manifest)
	if manifestErr != nil {
		return result, manifestErr
	}
	if len(artifactErrs) > 0 {
		return result, fmt.Errorf("write diagnostics artifacts: %w", errors.Join(artifactErrs...))
	}
	return result, nil
}

func (c *Capturer) captureTmux(req TmuxCapture) (Result, error) {
	stepDir, segments, manifest, err := c.startManifest(req.Identity, req.Failure, req.Transport)
	if err != nil {
		return Result{}, err
	}

	unlock := c.lockStep(stepDir)
	defer unlock()

	if err := os.MkdirAll(stepDir, 0o755); err != nil {
		return Result{}, err
	}

	var artifactErrs []error
	add := func(entry ArtifactEntry, err error) {
		manifest.Artifacts = append(manifest.Artifacts, entry)
		if err != nil {
			artifactErrs = append(artifactErrs, err)
		}
	}

	add(c.writeByteArtifact(stepDir, ArtifactParentStdout, "parent/stdout-tail.txt", mediaText, true, req.ParentStdout, ParentOutputLimit, boundTailBytes))
	add(c.writeByteArtifact(stepDir, ArtifactParentStderr, "parent/stderr-tail.txt", mediaText, true, req.ParentStderr, ParentOutputLimit, boundTailBytes))

	promptEntries, promptErrs := c.writePromptArtifacts(stepDir, req.Prompt, true)
	for _, entry := range promptEntries {
		manifest.Artifacts = append(manifest.Artifacts, entry)
	}
	artifactErrs = append(artifactErrs, promptErrs...)

	add(c.writeByteArtifact(stepDir, ArtifactTmuxPane, "tmux/pane.txt", mediaText, true, req.Pane, PaneByteLimit, func(src ByteArtifact, _ int) boundedBytes {
		return boundPaneBytes(src)
	}))
	add(c.writeTitleHistoryArtifact(stepDir, req.TitleHistory, req.TitleHistoryKnown, req.TitleHistoryError))

	tmuxMetadata := req.Metadata
	if tmuxMetadata.TitleReadError == "" {
		tmuxMetadata.TitleReadError = req.TitleHistoryError
	}
	add(c.writeJSONArtifact(stepDir, ArtifactTmuxMetadata, "tmux/metadata.json", true, tmuxMetadata, compactGenericMetadata))

	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactExecCommand, "exec/command.json", mediaJSON))
	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactExecStdout, "exec/stdout-tail.txt", mediaText))
	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactExecStderr, "exec/stderr-tail.txt", mediaText))
	manifest.Artifacts = append(manifest.Artifacts, notApplicable(ArtifactExecCombined, "exec/combined-tail.txt", mediaText))

	result, manifestErr := c.finishManifest(stepDir, segments, manifest)
	if manifestErr != nil {
		return result, manifestErr
	}
	if len(artifactErrs) > 0 {
		return result, fmt.Errorf("write diagnostics artifacts: %w", errors.Join(artifactErrs...))
	}
	return result, nil
}

func (c *Capturer) startManifest(id Identity, failure Failure, transport Transport) (string, pathSegments, Manifest, error) {
	if c == nil {
		c = New(".")
	}
	segments, err := derivePathSegments(id)
	if err != nil {
		return "", pathSegments{}, Manifest{}, err
	}
	stepDir := filepath.Join(c.debugRoot, segments.run, segments.task, segments.step)

	createdAt := c.now().UTC()
	capturedAt := failure.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = createdAt
	}

	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		CreatedAt:     createdAt,
		RunID:         id.RunID,
		TaskID:        id.TaskID,
		StepName:      id.StepName,
		PathSegments: ManifestPathSegments{
			Run:  segments.run,
			Task: segments.task,
			Step: segments.step,
		},
		Failure: ManifestFailure{
			Path:       strings.TrimSpace(failure.Path),
			Message:    boundFailureMessage(failure.Message),
			CapturedAt: capturedAt.UTC(),
		},
		Transport: transport,
	}
	return stepDir, segments, manifest, nil
}

func (c *Capturer) finishManifest(stepDir string, _ pathSegments, manifest Manifest) (Result, error) {
	manifestPath := filepath.Join(stepDir, "manifest.json")
	manifestEntry := ArtifactEntry{
		Kind:      ArtifactManifest,
		Path:      "manifest.json",
		MediaType: mediaJSON,
		Status:    ArtifactStatusWritten,
		Required:  true,
		Truncated: false,
	}
	manifest.Artifacts = append(manifest.Artifacts, manifestEntry)

	manifestIndex := len(manifest.Artifacts) - 1
	var data []byte
	var size int64
	for range 8 {
		manifest.Artifacts[manifestIndex].Bytes = int64Ptr(size)
		marshaled, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return Result{Dir: stepDir, ManifestPath: manifestPath, Manifest: manifest}, err
		}
		marshaled = append(marshaled, '\n')
		if int64(len(marshaled)) == size {
			data = marshaled
			break
		}
		size = int64(len(marshaled))
		data = marshaled
	}
	manifest.Artifacts[manifestIndex].Bytes = int64Ptr(int64(len(data)))
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{Dir: stepDir, ManifestPath: manifestPath, Manifest: manifest}, err
	}
	data = append(data, '\n')
	manifest.Artifacts[manifestIndex].Bytes = int64Ptr(int64(len(data)))

	if err := writeAtomic(manifestPath, data, 0o644); err != nil {
		return Result{Dir: stepDir, ManifestPath: manifestPath, Manifest: manifest}, err
	}
	return Result{Dir: stepDir, ManifestPath: manifestPath, Manifest: manifest}, nil
}

func (c *Capturer) lockStep(stepDir string) func() {
	c.mu.Lock()
	lock := c.locks[stepDir]
	if lock == nil {
		lock = &sync.Mutex{}
		c.locks[stepDir] = lock
	}
	c.mu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func (c *Capturer) writeByteArtifact(stepDir, kind, relPath, mediaType string, required bool, src ByteArtifact, limit int, bound func(ByteArtifact, int) boundedBytes) (ArtifactEntry, error) {
	entry := ArtifactEntry{
		Kind:       kind,
		Path:       relPath,
		MediaType:  mediaType,
		Required:   required,
		LimitBytes: int64(limit),
		Truncated:  false,
	}
	if kind == ArtifactTmuxPane {
		entry.LimitLines = PaneLineLimit
	}
	if !src.Available {
		entry.Status = ArtifactStatusUnavailable
		entry.Reason = "artifact data was not provided"
		return entry, nil
	}

	bounded := bound(src, limit)
	entry.Status = ArtifactStatusWritten
	entry.Bytes = int64Ptr(int64(len(bounded.data)))
	entry.Truncated = bounded.truncated
	entry.LimitBytes = bounded.limitBytes
	if bounded.limitLines > 0 {
		entry.LimitLines = bounded.limitLines
	}
	if bounded.truncated {
		entry.OriginalBytes = bounded.originalBytes
		entry.OriginalLines = bounded.originalLines
		entry.SHA256 = bounded.sha256
	}

	if err := writeAtomic(filepath.Join(stepDir, relPath), bounded.data, 0o644); err != nil {
		entry.Status = ArtifactStatusFailed
		entry.Error = err.Error()
		entry.Bytes = nil
		return entry, err
	}
	return entry, nil
}

func (c *Capturer) writePromptArtifacts(stepDir string, prompt PromptPayload, required bool) ([]ArtifactEntry, []error) {
	entries := make([]ArtifactEntry, 0, 3)
	var errs []error

	rawEntry, rawBound, err := c.writePromptByteArtifact(stepDir, ArtifactPromptRaw, "prompt/raw.bin", mediaOctetStream, required, prompt.Raw, false)
	entries = append(entries, rawEntry)
	if err != nil {
		errs = append(errs, err)
	}

	normalized := prompt.Normalized
	if !normalized.Available && prompt.Raw.Available {
		normalized = Bytes(NormalizePromptText(prompt.Raw.Data))
	}
	normalEntry, normalBound, err := c.writePromptByteArtifact(stepDir, ArtifactPromptNormal, "prompt/normalized.txt", mediaText, required, normalized, true)
	entries = append(entries, normalEntry)
	if err != nil {
		errs = append(errs, err)
	}

	metadata := buildPromptMetadata(prompt, rawBound, normalBound, rawEntry, normalEntry)
	metadataEntry, err := c.writeJSONArtifact(stepDir, ArtifactPromptMetadata, "prompt/metadata.json", required, metadata, compactPromptMetadata)
	entries = append(entries, metadataEntry)
	if err != nil {
		errs = append(errs, err)
	}

	return entries, errs
}

func (c *Capturer) writePromptByteArtifact(stepDir, kind, relPath, mediaType string, required bool, src ByteArtifact, normalize bool) (ArtifactEntry, boundedBytes, error) {
	entry := ArtifactEntry{
		Kind:       kind,
		Path:       relPath,
		MediaType:  mediaType,
		Required:   required,
		LimitBytes: PromptByteLimit,
		Truncated:  false,
	}
	if !src.Available {
		entry.Status = ArtifactStatusUnavailable
		entry.Reason = "artifact data was not provided"
		return entry, boundedBytes{limitBytes: PromptByteLimit}, nil
	}
	if normalize {
		src = Bytes(NormalizePromptText(src.Data))
	}

	bounded := boundPromptBytes(src, PromptByteLimit)
	entry.Status = ArtifactStatusWritten
	entry.Bytes = int64Ptr(int64(len(bounded.data)))
	entry.Truncated = bounded.truncated
	if bounded.truncated {
		entry.OriginalBytes = bounded.originalBytes
		entry.SHA256 = bounded.sha256
	}
	if err := writeAtomic(filepath.Join(stepDir, relPath), bounded.data, 0o644); err != nil {
		entry.Status = ArtifactStatusFailed
		entry.Error = err.Error()
		entry.Bytes = nil
		return entry, bounded, err
	}
	return entry, bounded, nil
}

func (c *Capturer) writeTitleHistoryArtifact(stepDir string, events []TitleEvent, available bool, titleErr string) (ArtifactEntry, error) {
	entry := ArtifactEntry{
		Kind:        ArtifactTmuxTitles,
		Path:        "tmux/title-history.jsonl",
		MediaType:   "application/x-ndjson",
		Required:    true,
		LimitEvents: TitleEventLimit,
		LimitBytes:  TitleBytesLimit,
		Truncated:   false,
	}
	if !available {
		entry.Status = ArtifactStatusUnavailable
		entry.Reason = "title history was not provided"
		if titleErr != "" {
			entry.Error = titleErr
		}
		return entry, nil
	}

	originalEvents := len(events)
	if originalEvents > TitleEventLimit {
		events = events[originalEvents-TitleEventLimit:]
		entry.Truncated = true
		entry.OriginalEvents = originalEvents
	}

	var originalTitleBytes int64
	var out []byte
	for _, event := range events {
		boundedTitle, truncated, originalBytes := truncateStringBytes(event.Title, TitleBytesLimit)
		if truncated {
			entry.Truncated = true
			originalTitleBytes += int64(originalBytes)
		}
		event.Title = boundedTitle
		line, err := json.Marshal(event)
		if err != nil {
			entry.Status = ArtifactStatusFailed
			entry.Error = err.Error()
			return entry, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	if originalTitleBytes > 0 {
		entry.OriginalBytes = originalTitleBytes
	}
	entry.Status = ArtifactStatusWritten
	entry.Bytes = int64Ptr(int64(len(out)))
	if err := writeAtomic(filepath.Join(stepDir, entry.Path), out, 0o644); err != nil {
		entry.Status = ArtifactStatusFailed
		entry.Error = err.Error()
		entry.Bytes = nil
		return entry, err
	}
	return entry, nil
}

func (c *Capturer) writeJSONArtifact(stepDir, kind, relPath string, required bool, value any, compact func(any) any) (ArtifactEntry, error) {
	entry := ArtifactEntry{
		Kind:       kind,
		Path:       relPath,
		MediaType:  mediaJSON,
		Status:     ArtifactStatusWritten,
		Required:   required,
		LimitBytes: JSONMetadataLimit,
		Truncated:  false,
	}

	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		entry.Status = ArtifactStatusFailed
		entry.Error = err.Error()
		return entry, err
	}
	data = append(data, '\n')
	originalBytes := len(data)
	if len(data) > JSONMetadataLimit {
		entry.Truncated = true
		entry.OriginalBytes = int64(originalBytes)
		entry.SHA256 = sha256Hex(data)
		value = compact(value)
		data, err = json.MarshalIndent(value, "", "  ")
		if err != nil {
			entry.Status = ArtifactStatusFailed
			entry.Error = err.Error()
			return entry, err
		}
		data = append(data, '\n')
		if len(data) > JSONMetadataLimit {
			value = map[string]any{
				"truncated":            true,
				"reason":               "metadata exceeded diagnostics JSON budget",
				"omitted_large_fields": []string{"metadata"},
				"limit_bytes":          JSONMetadataLimit,
				"original_bytes":       originalBytes,
			}
			data, err = json.MarshalIndent(value, "", "  ")
			if err != nil {
				entry.Status = ArtifactStatusFailed
				entry.Error = err.Error()
				return entry, err
			}
			data = append(data, '\n')
		}
	}

	entry.Bytes = int64Ptr(int64(len(data)))
	if err := writeAtomic(filepath.Join(stepDir, relPath), data, 0o644); err != nil {
		entry.Status = ArtifactStatusFailed
		entry.Error = err.Error()
		entry.Bytes = nil
		return entry, err
	}
	return entry, nil
}

func notApplicable(kind, relPath, mediaType string) ArtifactEntry {
	return ArtifactEntry{
		Kind:      kind,
		Path:      relPath,
		MediaType: mediaType,
		Status:    ArtifactStatusNotApplicable,
		Required:  false,
		Truncated: false,
		Reason:    "artifact does not apply to this transport",
	}
}

func defaultExecTransport(transport Transport) Transport {
	if strings.TrimSpace(transport.Kind) == "" {
		transport.Kind = "exec"
	}
	return transport
}

func defaultTmuxTransport(transport Transport) Transport {
	if strings.TrimSpace(transport.Kind) == "" {
		transport.Kind = "tmux"
	}
	transport.Interactive = true
	return transport
}

func buildPromptMetadata(prompt PromptPayload, rawBound, normalBound boundedBytes, rawEntry, normalEntry ArtifactEntry) map[string]any {
	mode := strings.TrimSpace(prompt.NormalizationMode)
	if mode == "" {
		mode = "utf8_replacement_lf"
	}
	metadata := map[string]any{
		"raw_bytes":                rawBound.originalBytes,
		"normalized_bytes":         normalBound.originalBytes,
		"raw_written_bytes":        bytesValue(rawEntry.Bytes),
		"normalized_written_bytes": bytesValue(normalEntry.Bytes),
		"normalization_mode":       mode,
		"bracketed_paste":          prompt.BracketedPaste,
		"limit_bytes":              PromptByteLimit,
		"truncation": map[string]any{
			"raw":        rawEntry.Truncated,
			"normalized": normalEntry.Truncated,
		},
	}
	if prompt.ExtraMetadata != nil {
		metadata["extra"] = prompt.ExtraMetadata
	}
	return metadata
}

func sanitizeExecCommand(command ExecCommand) ExecCommand {
	out := command
	out.Argv = append([]string(nil), command.Argv...)
	out.Env.InheritedKeys = append([]string(nil), command.Env.InheritedKeys...)
	sort.Strings(out.Env.InheritedKeys)

	out.Env.Step = make(map[string]string, len(command.Env.Step))
	patterns := out.Redaction.SensitiveNamePatterns
	if len(patterns) == 0 {
		patterns = []string{"TOKEN", "PASSWORD", "SECRET", "KEY", "CREDENTIAL"}
	}
	for k, v := range command.Env.Step {
		if sensitiveName(k, patterns) {
			out.Env.Step[k] = "[REDACTED]"
			continue
		}
		out.Env.Step[k] = v
	}
	if len(out.Env.Step) == 0 {
		out.Env.Step = nil
	}
	out.Redaction = RedactionMetadata{
		SensitiveNamePatterns: append([]string(nil), patterns...),
		ValuesRedacted:        true,
	}
	return out
}

func sensitiveName(name string, patterns []string) bool {
	upper := strings.ToUpper(name)
	for _, pattern := range patterns {
		pattern = strings.ToUpper(strings.TrimSpace(pattern))
		if pattern != "" && strings.Contains(upper, pattern) {
			return true
		}
	}
	return false
}

func compactExecCommand(value any) any {
	command, ok := value.(ExecCommand)
	if !ok {
		return compactGenericMetadata(value)
	}
	const maxArgs = 64
	const maxEnv = 64
	const maxInherited = 256
	command.Argv = compactStringSlice(command.Argv, maxArgs, 2048)
	command.WorkingDir = compactString(command.WorkingDir, 2048)
	command.Env.InheritedKeys = compactStringSlice(command.Env.InheritedKeys, maxInherited, 512)
	command.Env.Step = compactStringMap(command.Env.Step, maxEnv, 2048)
	if command.Extra != nil {
		command.Extra = map[string]any{"omitted": "large exec command metadata omitted"}
	}
	return command
}

func compactGenericMetadata(value any) any {
	return map[string]any{
		"truncated": true,
		"summary":   "large diagnostics metadata omitted",
	}
}

func compactPromptMetadata(value any) any {
	metadata, ok := value.(map[string]any)
	if !ok {
		return compactGenericMetadata(value)
	}
	out := make(map[string]any, len(metadata))
	for key, val := range metadata {
		if key == "extra" {
			out["omitted_large_fields"] = []string{"extra"}
			continue
		}
		out[key] = val
	}
	out["truncated"] = true
	return out
}

func compactStringSlice(values []string, maxCount int, maxBytes int) []string {
	if len(values) > maxCount {
		values = values[:maxCount]
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = compactString(value, maxBytes)
	}
	return out
}

func compactStringMap(values map[string]string, maxCount int, maxBytes int) map[string]string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxCount {
		keys = keys[:maxCount]
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = compactString(values[key], maxBytes)
	}
	return out
}

func compactString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= len(titleElisionMarker) {
		return value[:limit]
	}
	return value[:limit-len(titleElisionMarker)] + titleElisionMarker
}

func boundFailureMessage(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len([]byte(message)) <= FailureMessageLimit {
		return message
	}
	truncated, _, _ := truncateStringBytes(message, FailureMessageLimit)
	return truncated
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func int64Ptr(v int64) *int64 {
	return &v
}

func bytesValue(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
