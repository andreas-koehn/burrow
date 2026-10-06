package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
)

var (
	// ErrInvalidModel is wrapped with the reason a synthetic model was
	// refused; the reason is safe to show to an admin.
	ErrInvalidModel = errors.New("store: invalid model")
	// ErrModelExists: another model already has the name.
	ErrModelExists = errors.New("store: model name already in use")
	// ErrModelNotFound: no model has that name.
	ErrModelNotFound = errors.New("store: model not found")
	// ErrProviderInUse is wrapped with the names of the models that target
	// the provider.
	ErrProviderInUse = errors.New("store: provider is used by a model")
)

var modelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)

// ValidModelName reports whether s can name a synthetic model. Names never
// contain "/", which keeps them apart from "<provider>/<model>" addresses.
// Go's $ matches only at the end of the text, so a trailing newline is refused.
func ValidModelName(s string) bool {
	return modelNameRe.MatchString(s) && s != "v1"
}

const (
	maxModelTargets     = 8 // per dialect
	maxModelDescription = 500
	maxTargetModel      = 200
	defaultAttemptSecs  = 60
	defaultTotalSecs    = 120
	maxTimeoutSecs      = 600
)

// modelDialects are the API formats a target can have.
var modelDialects = []string{"openai", "anthropic"}

func invalidModel(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidModel, reason) }

// normalizeModel validates m and fills defaults: a target's empty dialect
// becomes its provider's format, timeouts of 0 become the defaults, and
// positions are numbered per dialect in the order the targets were given.
// Reasons name a field or a provider slug, never free text of the caller.
func (s *Store) normalizeModel(ctx context.Context, m db.AIModel) (db.AIModel, error) {
	if !ValidModelName(m.Name) {
		return m, invalidModel("name must be 2-63 characters: lowercase letters, digits, dot, underscore, hyphen")
	}
	if _, err := s.q.GetAIProvider(ctx, m.Name); err == nil {
		return m, invalidModel("name is already a provider slug")
	} else if !errors.Is(err, db.ErrNotFound) {
		return m, err
	}
	m.Description = strings.TrimSpace(m.Description)
	if len(m.Description) > maxModelDescription || hasControl(m.Description) {
		return m, invalidModel("description must be at most 500 characters without control characters")
	}
	const targetCount = "a model needs at least one target and at most 8 per format"
	if len(m.Targets) == 0 || len(m.Targets) > maxModelTargets*len(modelDialects) {
		return m, invalidModel(targetCount)
	}
	formats := map[string]string{} // provider slug -> api_format
	next := map[string]int{}
	targets := make([]db.AIModelTarget, 0, len(m.Targets))
	for _, t := range m.Targets {
		if !ValidProviderSlug(t.ProviderSlug) {
			return m, invalidModel("a target names an invalid provider slug")
		}
		format, known := formats[t.ProviderSlug]
		if !known {
			p, err := s.q.GetAIProvider(ctx, t.ProviderSlug)
			if errors.Is(err, db.ErrNotFound) {
				return m, invalidModel("unknown provider " + t.ProviderSlug)
			}
			if err != nil {
				return m, err
			}
			format = p.APIFormat
			formats[t.ProviderSlug] = format
		}
		switch t.Dialect {
		case "":
			t.Dialect = format
		case "openai", "anthropic":
		default:
			return m, invalidModel("a target's format must be 'openai' or 'anthropic'")
		}
		if t.Dialect != format {
			return m, invalidModel("provider " + t.ProviderSlug + " speaks " + format + ", not " + t.Dialect)
		}
		t.TargetModel = strings.TrimSpace(t.TargetModel)
		if t.TargetModel == "" || len(t.TargetModel) > maxTargetModel || hasControl(t.TargetModel) {
			return m, invalidModel("a target's model must be 1-200 characters without control characters")
		}
		t.Position = next[t.Dialect]
		next[t.Dialect]++
		if next[t.Dialect] > maxModelTargets {
			return m, invalidModel(targetCount)
		}
		targets = append(targets, t)
	}
	m.Targets = targets
	if m.AttemptTimeoutS == 0 {
		m.AttemptTimeoutS = defaultAttemptSecs
	}
	if m.TotalTimeoutS == 0 {
		m.TotalTimeoutS = defaultTotalSecs
	}
	if m.AttemptTimeoutS < 1 || m.AttemptTimeoutS > maxTimeoutSecs || m.TotalTimeoutS < 1 || m.TotalTimeoutS > maxTimeoutSecs {
		return m, invalidModel("timeouts must be between 1 and 600 seconds")
	}
	if m.TotalTimeoutS < m.AttemptTimeoutS {
		return m, invalidModel("total timeout must not be shorter than the attempt timeout")
	}
	return m, nil
}

// ListModels returns every synthetic model ordered by name, with targets.
func (s *Store) ListModels(ctx context.Context) ([]db.AIModel, error) {
	return s.q.ListAIModels(ctx)
}

// ModelByName returns the model with its targets, or ErrModelNotFound. No
// permission gate: the gateway resolves models on the request path.
func (s *Store) ModelByName(ctx context.Context, name string) (db.AIModel, error) {
	m, err := s.q.GetAIModel(ctx, name)
	if errors.Is(err, db.ErrNotFound) {
		return db.AIModel{}, ErrModelNotFound
	}
	return m, err
}

// CreateModel adds a synthetic model. Callers gate on admin; the store
// validates the data.
func (s *Store) CreateModel(ctx context.Context, m db.AIModel) (db.AIModel, error) {
	m, err := s.normalizeModel(ctx, m)
	if err != nil {
		return db.AIModel{}, err
	}
	if err := s.q.CreateAIModel(ctx, m); err != nil {
		if errors.Is(err, db.ErrDuplicateModel) {
			return db.AIModel{}, ErrModelExists
		}
		return db.AIModel{}, err
	}
	return s.ModelByName(ctx, m.Name)
}

// UpdateModel replaces the fields and targets of the model called name;
// m.Name may differ, which renames it.
func (s *Store) UpdateModel(ctx context.Context, name string, m db.AIModel) (db.AIModel, error) {
	m, err := s.normalizeModel(ctx, m)
	if err != nil {
		return db.AIModel{}, err
	}
	switch err := s.q.UpdateAIModel(ctx, name, m); {
	case errors.Is(err, db.ErrNotFound):
		return db.AIModel{}, ErrModelNotFound
	case errors.Is(err, db.ErrDuplicateModel):
		return db.AIModel{}, ErrModelExists
	case err != nil:
		return db.AIModel{}, err
	}
	return s.ModelByName(ctx, m.Name)
}

// DeleteModel removes the model and its targets.
func (s *Store) DeleteModel(ctx context.Context, name string) error {
	if err := s.q.DeleteAIModel(ctx, name); errors.Is(err, db.ErrNotFound) {
		return ErrModelNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// slugTakenByModel reports whether a synthetic model has the name slug. Model
// names and provider slugs share one namespace: an allow-list entry without
// "/" names a model, and a model cannot be told from a provider otherwise.
func (s *Store) slugTakenByModel(ctx context.Context, slug string) (bool, error) {
	if _, err := s.q.GetAIModel(ctx, slug); errors.Is(err, db.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// modelAliasesImportedKey marks, in the settings table, that the one-time
// import has completed. It is not in the settings API's key whitelist.
const modelAliasesImportedKey = "ai_models.aliases_imported"

// ImportModelAliases turns the model aliases of earlier versions into
// synthetic models: one model per alias name, with one target per alias row
// whose service backs a provider, ordered by priority and then by age. An
// alias is skipped when its name is not a valid model name, when a model or a
// provider already has the name, or when none of its services backs a
// provider. The alias rows themselves are left as they are.
//
// Like BackfillAIProviders it does its work once per database and leaves a
// marker: the alias rows stay, so without the marker a model an admin deleted
// would come back at the next start. Safe to call at every start.
func (s *Store) ImportModelAliases(ctx context.Context) (int, error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return 0, err
	}
	if settings[modelAliasesImportedKey] != "" {
		return 0, nil
	}
	aliases, err := s.q.ListModelAliases(ctx)
	if err != nil {
		return 0, err
	}
	sort.SliceStable(aliases, func(i, j int) bool {
		a, b := aliases[i], aliases[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	var names []string
	targets := map[string][]db.AIModelTarget{}
	providers := map[string]db.AIProvider{} // by service id; a zero value means none
	for _, a := range aliases {
		if !ValidModelName(a.Alias) {
			continue
		}
		p, seen := providers[a.ServiceID]
		if !seen {
			p, err = s.q.GetAIProviderByService(ctx, a.ServiceID)
			if err != nil && !errors.Is(err, db.ErrNotFound) {
				return 0, err
			}
			providers[a.ServiceID] = p
		}
		if p.Slug == "" {
			continue
		}
		if _, ok := targets[a.Alias]; !ok {
			names = append(names, a.Alias)
		}
		targets[a.Alias] = append(targets[a.Alias], db.AIModelTarget{Dialect: p.APIFormat, ProviderSlug: p.Slug, TargetModel: a.ConcreteModel})
	}
	sort.Strings(names)
	created := 0
	for _, name := range names {
		_, err := s.CreateModel(ctx, db.AIModel{Name: name, Enabled: true, Targets: targets[name]})
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrModelExists), errors.Is(err, ErrInvalidModel):
			// Taken by a model or a provider, or not expressible as a model.
		default:
			return created, err
		}
	}
	if err := s.SaveSettings(ctx, map[string]string{modelAliasesImportedKey: "1"}); err != nil {
		return created, err
	}
	return created, nil
}
