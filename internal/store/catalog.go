package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/mickey-kras/gpu-workload-supervisor/internal/control"
	"reflect"
)

var ErrCatalogReferenced = errors.New("workload profile is referenced by live state or unfinished work")
var ErrCatalogRevisionNotFound = errors.New("no catalog recorded at the requested revision")

func readCatalog(ctx context.Context, q querier) (control.CatalogSnapshot, error) {
	var s control.CatalogSnapshot
	var b []byte
	err := q.QueryRowContext(ctx, "SELECT revision, catalog FROM workload_catalog WHERE singleton=1").Scan(&s.Revision, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.Catalog, err = control.DecodeCatalogBytes(b)
	return s, err
}
func (s *Store) Catalog(ctx context.Context) (control.CatalogSnapshot, error) {
	return readCatalog(ctx, s.db)
}

// CatalogAtRevision reads a historical catalog blob, giving owned-unit cleanup
// and audit a recoverable content proof for profiles the current catalog no
// longer carries.
func (s *Store) CatalogAtRevision(ctx context.Context, revision string) (control.CatalogSnapshot, error) {
	var s2 control.CatalogSnapshot
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT revision, catalog FROM workload_catalog_history WHERE revision=?", revision).Scan(&s2.Revision, &b)
	if errors.Is(err, sql.ErrNoRows) {
		return s2, ErrCatalogRevisionNotFound
	}
	if err != nil {
		return s2, err
	}
	s2.Catalog, err = control.DecodeCatalogBytes(b)
	return s2, err
}

// ReplaceCatalog is called while holding the controller gate. The writer
// transaction checks references and atomically advances catalog and state token.
func (s *Store) ReplaceCatalog(ctx context.Context, expected string, c control.Catalog) (control.CatalogSnapshot, error) {
	if err := c.Validate(); err != nil {
		return control.CatalogSnapshot{}, err
	}
	c = c.Clone()
	var old control.CatalogSnapshot
	var revision string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		old, revision, err = s.replaceCatalog(ctx, tx, expected, c)
		return err
	})
	if err != nil {
		return old, err
	}
	return control.CatalogSnapshot{Revision: revision, Catalog: c}, nil
}

func (s *Store) replaceCatalog(ctx context.Context, tx *sql.Tx, expected string, c control.Catalog) (control.CatalogSnapshot, string, error) {
	if err := temporaryDiscoveryPending(ctx, tx); err != nil {
		return control.CatalogSnapshot{}, "", err
	}
	old, err := readCatalog(ctx, tx)
	if err != nil {
		return old, "", err
	}
	if old.Revision != expected {
		return old, "", ErrVersionConflict
	}
	if err := validateCatalogVersionTransition(old.Catalog, c); err != nil {
		return old, "", err
	}
	state, err := readState(ctx, tx)
	if err != nil {
		return old, "", err
	}
	refs := map[control.Workload]bool{state.ActiveWorkload: true, state.DesiredWorkload: true}
	if err := readCatalogWorkReferences(ctx, tx, refs); err != nil {
		return old, "", err
	}
	if err := validateCatalogReferences(c, old, refs); err != nil {
		return old, "", err
	}
	revision, err := s.uuid()
	if err != nil {
		return old, "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return old, "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO workload_catalog(singleton,revision,catalog) VALUES(1,?,?) ON CONFLICT(singleton) DO UPDATE SET revision=excluded.revision,catalog=excluded.catalog", revision, b); err != nil {
		return old, "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO workload_catalog_history(revision,catalog) VALUES(?,?)", revision, b); err != nil {
		return old, "", err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE control_state SET version=version+1,updated_at=? WHERE singleton=1", formatTime(s.now())); err != nil {
		return old, "", err
	}
	return old, revision, nil
}

// validateCatalogVersionTransition permits 1→1, 1→2 and 2→2. A 2→1 downgrade
// is safe only when the new catalog carries no owned profiles; owned profiles
// in a v1 catalog are already rejected by Validate, so this guard is the
// defense-in-depth layer behind it.
func validateCatalogVersionTransition(old, next control.Catalog) error {
	if old.Version != 2 || next.Version != 1 {
		return nil
	}
	for _, p := range next.Profiles {
		if p.NativeModel != nil && p.NativeModel.Owned != nil {
			return control.ErrOwnedCatalogManagedBySetup
		}
	}
	return nil
}

// readCatalogWorkReferences uses the replacement transaction's snapshot so live
// work and in-progress transitions cannot lose their referenced profiles.
func readCatalogWorkReferences(ctx context.Context, tx *sql.Tx, refs map[control.Workload]bool) error {
	rows, err := tx.QueryContext(ctx, "SELECT workload FROM registered_work WHERE completed_at IS NULL AND workload IS NOT NULL")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id control.Workload
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		refs[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, "SELECT source_state,target_state,previous_state FROM transitions WHERE status='in_progress'")
	if err != nil {
		return err
	}
	for rows.Next() {
		var a, b, d []byte
		if err = rows.Scan(&a, &b, &d); err != nil {
			break
		}
		err = addTransitionReferences(refs, a, b, d)
		if err != nil {
			break
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	return nil
}

func validateCatalogReferences(c control.Catalog, old control.CatalogSnapshot, refs map[control.Workload]bool) error {
	for id := range refs {
		if id == control.WorkloadIdle || id == control.WorkloadUnknown {
			continue
		}
		p, ok := c.Profile(id)
		prior, had := old.Catalog.Profile(id)
		if !ok || had && !reflect.DeepEqual(p, prior) || !had && old.Revision == "" {
			return ErrCatalogReferenced
		}
	}
	return nil
}

func addTransitionReferences(refs map[control.Workload]bool, states ...[]byte) error {
	for _, raw := range states {
		var st control.State
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		refs[st.ActiveWorkload] = true
		refs[st.DesiredWorkload] = true
	}
	return nil
}
