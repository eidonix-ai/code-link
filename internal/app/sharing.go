package app

import (
	"os"

	"codelink/internal/config"
	"codelink/internal/share"
)

// ExportTo writes the saved settings to a .codelink file (never keys, the
// token, the port or launch settings).
func (a *App) ExportTo(path string) error {
	return os.WriteFile(path, share.Marshal(share.Export(a.Config())), 0o644)
}

// ImportPreview is what a .codelink file would bring.
type ImportPreview struct {
	share.Preview
	// Data is the file, handed back to ImportInto.
	Data string `json:"data"`
}

// PreviewImport reads a .codelink file for the preview.
func PreviewImport(data []byte) (ImportPreview, error) {
	f, err := share.Parse(data)
	if err != nil {
		return ImportPreview{}, err
	}
	return ImportPreview{Preview: share.Summarize(f), Data: string(data)}, nil
}

// ImportResult is a merged draft and the keys it still needs.
type ImportResult struct {
	View  View      `json:"view"`
	Needs []KeyNeed `json:"needs"`
}

// ImportInto merges a .codelink file into a draft (not saved).
func (a *App) ImportInto(d config.Config, data []byte) (ImportResult, error) {
	f, err := share.Parse(data)
	if err != nil {
		return ImportResult{}, err
	}
	merged, res := share.Merge(d, f)
	// The file's main model becomes the default model when it isn't just
	// the server's recommendation.
	if m := merged.Roles.Main; m != "" && m != Recommendation(&merged) {
		merged.Launch.DefaultModel = m
	}
	return ImportResult{View: a.Describe(merged), Needs: Needs(&merged, res.NeedKey)}, nil
}
