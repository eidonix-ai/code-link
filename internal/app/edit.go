package app

import "codelink/internal/config"

// View is the Settings page's draft with what it derives from it: the model
// pickers (enabled models and the ones the last probe found), the decision
// model's logprob note and a warning when an edit cleared roles.
type View struct {
	Draft    config.Config `json:"draft"`
	Groups   []ChoiceGroup `json:"groups"`
	Decision string        `json:"decision"`
	Warning  string        `json:"warning"`
	// Search and Documents are the pickers' choices and current values.
	Search         []Option `json:"search"`
	SearchValue    string   `json:"searchValue"`
	Documents      []Option `json:"documents"`
	DocumentsValue string   `json:"documentsValue"`
}

// Describe derives the view of a draft.
func (a *App) Describe(d config.Config) View {
	probes := a.Probes()
	if d.Backends == nil {
		d.Backends = []config.Backend{}
	}
	if d.Models == nil {
		d.Models = []config.Model{}
	}
	return View{Draft: d, Groups: Groups(&d, probes, false), Decision: DecisionNote(&d, probes),
		Search: SearchOptions(&d, probes), SearchValue: SearchChoice(&d),
		Documents: DocumentsOptions(&d, probes), DocumentsValue: DocumentsChoice(&d)}
}

func (a *App) warn(d config.Config, w string) View {
	v := a.Describe(d)
	v.Warning = w
	return v
}

// EditSetRole points a slot at a provider model by tag ("" clears it).
func (a *App) EditSetRole(d config.Config, s Slot, tag string) View {
	if ch, ok := ParseTag(tag); ok {
		SetRole(&d, s, &ch, ProbedFor(a.Probes(), ch))
	} else {
		SetRole(&d, s, nil, nil)
	}
	return a.Describe(d)
}

// EditSetEnabled ticks or unticks a provider's model; unticking one in use
// clears its roles, with a warning.
func (a *App) EditSetEnabled(d config.Config, tag string, on bool) View {
	ch, ok := ParseTag(tag)
	if !ok {
		return a.Describe(d)
	}
	var id string
	if i := findModel(&d, ch); i >= 0 {
		id = d.Models[i].ID
	}
	cleared := SetEnabled(&d, ch, on, ProbedFor(a.Probes(), ch))
	return a.warn(d, ClearedWarning(cleared, id+" was turned off"))
}

// EditRenameModel changes a model's alias; a refused name leaves it.
func (a *App) EditRenameModel(d config.Config, old, name string) View {
	RenameModel(&d, old, name)
	return a.Describe(d)
}

// EditAddBackend adds a provider.
func (a *App) EditAddBackend(d config.Config) View {
	AddBackend(&d)
	return a.Describe(d)
}

// EditRenameBackend renames a provider; a refused name leaves it.
func (a *App) EditRenameBackend(d config.Config, old, name string) View {
	if RenameBackend(&d, old, name) {
		a.RenameProbe(old, name)
	}
	return a.Describe(d)
}

// EditRemoveBackend removes a provider and its models, warning about the
// roles that lost their model.
func (a *App) EditRemoveBackend(d config.Config, id string) View {
	cleared := RemoveBackend(&d, id)
	return a.warn(d, ClearedWarning(cleared, "provider "+id+" was removed"))
}

// EditSetSearch applies the web search picker.
func (a *App) EditSetSearch(d config.Config, value string) View {
	SetSearchChoice(&d, value)
	return a.Describe(d)
}

// EditSetDocuments applies the documents picker.
func (a *App) EditSetDocuments(d config.Config, value string) View {
	SetDocumentsChoice(&d, value)
	return a.Describe(d)
}
