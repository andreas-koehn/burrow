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
	// the provider. Deleting a provider, the service behind it or the user
	// who owns that service all return it; it is the database layer's error,
	// which finds the models when the delete fails on the foreign key.
	ErrProviderInUse = db.ErrProviderInUse
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

// ErrProvidersNotBackfilled is returned by ImportModelAliases while the
// provider backfill has not completed on this database.
var ErrProvidersNotBackfilled = errors.New("store: providers are not backfilled yet")

// aliasModels groups alias rows into one model per alias name. A row becomes
// a target when its service backs a provider; the target's dialect is that
// provider's format. Targets are ordered by priority, then age, then service
// id and model, so rows that tie on priority and time still come out in one
// order. Models are returned sorted by name. skipped lists, sorted, the
// aliases that give no model: a name that is not a valid model name, or no
// row whose service backs a provider.
func aliasModels(aliases []db.ModelAlias, providerOf func(serviceID string) (db.AIProvider, bool)) (models []db.AIModel, skipped []string) {
	rows := append([]db.ModelAlias{}, aliases...)
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.Alias != b.Alias:
			return a.Alias < b.Alias
		case a.Priority != b.Priority:
			return a.Priority < b.Priority
		case !a.CreatedAt.Equal(b.CreatedAt):
			return a.CreatedAt.Before(b.CreatedAt)
		case a.ServiceID != b.ServiceID:
			return a.ServiceID < b.ServiceID
		}
		return a.ConcreteModel < b.ConcreteModel
	})
	for i := 0; i < len(rows); {
		name := rows[i].Alias
		var targets []db.AIModelTarget
		for ; i < len(rows) && rows[i].Alias == name; i++ {
			if p, ok := providerOf(rows[i].ServiceID); ok {
				targets = append(targets, db.AIModelTarget{Dialect: p.APIFormat, ProviderSlug: p.Slug, TargetModel: rows[i].ConcreteModel})
			}
		}
		if !ValidModelName(name) || len(targets) == 0 {
			skipped = append(skipped, name)
			continue
		}
		models = append(models, db.AIModel{Name: name, Enabled: true, Targets: targets})
	}
	return models, skipped
}

// ImportModelAliases turns the model aliases of earlier versions into
// synthetic models: one model per alias name, with one target per alias row
// whose service backs a provider (see aliasModels). It returns the number of
// models created and the sorted names of the aliases that gave none: the
// name is not a valid model name, a model or a provider already has it, none
// of its services backs a provider, or the result is not a valid model (more
// than 8 targets in one format, a target model that is too long). The alias
// rows themselves are left as they are.
//
// Like BackfillAIProviders it does its work once per database and leaves a
// marker: the alias rows stay, so without the marker a model an admin deleted
// would come back at the next start. Because the run is final, it waits for
// the provider backfill: before that has completed there are no providers to
// target, every alias would be skipped for good, and the call returns
// ErrProvidersNotBackfilled without leaving the marker. A later call on a
// database that has the marker returns 0 and no names.
func (s *Store) ImportModelAliases(ctx context.Context) (created int, skipped []string, err error) {
	settings, err := s.GetSettings(ctx)
	if err != nil {
		return 0, nil, err
	}
	if settings[modelAliasesImportedKey] != "" {
		return 0, nil, nil
	}
	if settings[aiProvidersBackfilledKey] == "" {
		return 0, nil, ErrProvidersNotBackfilled
	}
	aliases, err := s.q.ListModelAliases(ctx)
	if err != nil {
		return 0, nil, err
	}
	providers := map[string]db.AIProvider{} // by service id
	for _, a := range aliases {
		if _, seen := providers[a.ServiceID]; seen {
			continue
		}
		p, err := s.q.GetAIProviderByService(ctx, a.ServiceID)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return 0, nil, err
		}
		providers[a.ServiceID] = p // the zero value when there is none
	}
	models, skipped := aliasModels(aliases, func(serviceID string) (db.AIProvider, bool) {
		p := providers[serviceID]
		return p, p.Slug != ""
	})
	for _, m := range models {
		_, err := s.CreateModel(ctx, m)
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrModelExists), errors.Is(err, ErrInvalidModel):
			// Taken by a model or a provider, or not expressible as a model.
			skipped = append(skipped, m.Name)
		default:
			return created, skipped, err
		}
	}
	sort.Strings(skipped)
	if err := s.SaveSettings(ctx, map[string]string{modelAliasesImportedKey: "1"}); err != nil {
		return created, skipped, err
	}
	return created, skipped, nil
}
