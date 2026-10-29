package collabflux

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"code-agent/internal/collab"
	"github.com/tuxi/flux-workflow/definition"
	"github.com/tuxi/flux-workflow/domain"
	"github.com/tuxi/flux-workflow/repository/query"
	"gorm.io/gorm"
)

// The pinned Flux registry rejects repeated names in one process. This private
// host publishes immutable versions through its existing repositories instead;
// workers load each task's exact version ID, never a mutable registry entry.
// The caller holds Service.mu and the exclusive managed host lock.
func (s *Service) templateBinding(ctx context.Context, def *definition.WorkflowDefinition) (collab.WorkflowBinding, error) {
	var binding collab.WorkflowBinding
	err := s.runtime.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		workflows := query.NewWorkflowRepository(tx)
		wf, err := workflows.GetByName(ctx, def.Name)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			wf = &domain.Workflow{Name: def.Name}
			err = workflows.Create(ctx, wf)
		}
		if err != nil {
			return err
		}
		var row struct{ ID int64 }
		err = tx.Table("workflow_versions").Select("id").Where("workflow_id=? AND hash=?", wf.ID, def.Hash()).Order("id DESC").Take(&row).Error
		versions := query.NewWorkflowVersionRepository(tx)
		var version *domain.WorkflowVersion
		if err == nil {
			version, err = versions.Get(ctx, row.ID)
		} else if errors.Is(err, gorm.ErrRecordNotFound) {
			next := int64(1)
			latest, e := versions.GetLatestByWorkflowID(ctx, wf.ID)
			if e == nil {
				if latest.Version == int64(^uint64(0)>>1) {
					return collab.ErrConflict
				}
				next = latest.Version + 1
			} else if !errors.Is(e, gorm.ErrRecordNotFound) {
				return e
			}
			raw, e := json.Marshal(def)
			if e != nil {
				return e
			}
			version = &domain.WorkflowVersion{WorkflowID: wf.ID, Version: next, Hash: def.Hash(), DefinitionJSON: raw}
			err = versions.Create(ctx, version)
		}
		if err != nil {
			return err
		}
		var stored definition.WorkflowDefinition
		if json.Unmarshal(version.DefinitionJSON, &stored) != nil || stored.Name != def.Name || stored.Hash() != def.Hash() {
			return collab.ErrConflict
		}
		binding = collab.WorkflowBinding{Name: def.Name, VersionID: strconv.FormatInt(version.ID, 10), Hash: version.Hash}
		return nil
	})
	return binding, err
}
