package router

import (
	"encoding/json"
	"sort"
	"strings"
)

// Modality names (models.dev input modality vocabulary, minus "text").
const (
	ModalityImage = "image"
	ModalityPDF   = "pdf"
	ModalityAudio = "audio"
	ModalityVideo = "video"
)

// DetectRequiredModalities scans a chat-completion body for non-text content
// blocks and returns the modalities the request needs, sorted and deduped
// (empty when the request is text-only or unparseable). Ported from 9router
// detectRequiredCapabilities: routing a request carrying an image to a
// text-only model wastes an attempt on a guaranteed 400.
func DetectRequiredModalities(body []byte) []string {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, m := range req.Messages {
		// String content is text-only; only arrays carry content blocks.
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if mod := blockModality(b); mod != "" {
				set[mod] = true
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for mod := range set {
		out = append(out, mod)
	}
	sort.Strings(out)
	return out
}

// blockModality maps one content block to the modality it requires ("" for
// text and unrecognized blocks).
func blockModality(b map[string]any) string {
	typ, _ := b["type"].(string)
	switch strings.ToLower(typ) {
	case "image_url", "image", "input_image":
		return ModalityImage
	case "input_audio", "audio_url", "audio":
		return ModalityAudio
	case "file", "document", "input_file":
		if mod := mimeModality(blockMIME(b)); mod != "" {
			return mod
		}
		// 9router fallback: a file block with no discoverable mime is treated
		// as a PDF — the overwhelmingly common document attachment.
		return ModalityPDF
	}
	return ""
}

// blockMIME digs the mime type out of a file/document block: an explicit
// media_type / mime_type at either level, or the "data:<mime>;base64," prefix
// of an embedded URL / file_data value.
func blockMIME(b map[string]any) string {
	for _, key := range []string{"media_type", "mime_type"} {
		if v, ok := b[key].(string); ok && v != "" {
			return v
		}
	}
	// Nested shapes: {"source": {...}}, {"file": {...}}, {"image_url": {...}}.
	for _, key := range []string{"source", "file", "document", "image_url"} {
		nested, ok := b[key].(map[string]any)
		if !ok {
			continue
		}
		if m := blockMIME(nested); m != "" {
			return m
		}
	}
	for _, key := range []string{"data", "url", "file_data", "file_url"} {
		if v, ok := b[key].(string); ok {
			if m := dataURIMIME(v); m != "" {
				return m
			}
		}
	}
	return ""
}

// dataURIMIME extracts the mime type from a "data:<mime>;base64,..." URI.
func dataURIMIME(v string) string {
	rest, ok := strings.CutPrefix(v, "data:")
	if !ok {
		return ""
	}
	mime, _, _ := strings.Cut(rest, ",")
	mime, _, _ = strings.Cut(mime, ";")
	return strings.TrimSpace(mime)
}

// mimeModality maps a mime type to a modality ("" when unrecognized).
func mimeModality(mime string) string {
	mime = strings.ToLower(mime)
	switch {
	case mime == "":
		return ""
	case strings.HasPrefix(mime, "image/"):
		return ModalityImage
	case mime == "application/pdf":
		return ModalityPDF
	case strings.HasPrefix(mime, "audio/"):
		return ModalityAudio
	case strings.HasPrefix(mime, "video/"):
		return ModalityVideo
	}
	return ""
}

// SetModalityLookup installs the model -> supported-input-modalities lookup
// (the models.dev catalog's synced entries). nil disables capability-aware
// reordering entirely.
func (r *Router) SetModalityLookup(f func(model string) ([]string, bool)) {
	r.modalities = f
}

// coversModalities reports whether the candidate's model is known to support
// every required modality.
//
// An unknown model (no catalog entry) does NOT cover a media requirement. That
// is the safe floor rather than a drop: the candidate keeps serving text-only
// requests normally, and is merely ranked last when media is required, so a
// route whose models are all uncatalogued still works exactly as before.
func (r *Router) coversModalities(model string, required []string) bool {
	if len(required) == 0 {
		return true
	}
	if r.modalities == nil {
		return true // no catalog data: ordering unchanged
	}
	supported, ok := r.modalities(model)
	if !ok {
		return false
	}
	for _, need := range required {
		found := false
		for _, have := range supported {
			if strings.EqualFold(have, need) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// reorderByModalities stable-sorts candidates that cover every required
// modality ahead of the rest. Never drops a candidate: an unusable model is
// still better than no answer when the catalog is wrong or stale.
func (r *Router) reorderByModalities(cands []Candidate, required []string) {
	if len(required) == 0 || len(cands) < 2 || r.modalities == nil {
		return
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return r.coversModalities(cands[i].Model, required) &&
			!r.coversModalities(cands[j].Model, required)
	})
}
