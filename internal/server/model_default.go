package server

import "code-agent/internal/settings"

// CLI --model and host Reconfigure may override the settings-file default.
// Report the same selection that an omitted-model Turn will actually receive.
func catalogWithRuntimeDefault(catalog RuntimeModelCatalog, mc *settings.ModelConfig) RuntimeModelCatalog {
	catalog.DefaultModel, catalog.DefaultRuntimeAlias = "", ""
	if mc == nil || mc.Model == "" {
		return catalog
	}
	catalog.DefaultModel = settings.ModelID(mc.Name, *mc)
	catalog.DefaultRuntimeAlias = mc.Name
	for _, c := range catalog.Connections {
		for _, m := range c.Models {
			if m.ID == catalog.DefaultModel {
				catalog.DefaultRuntimeAlias = m.RuntimeAlias
				return catalog
			}
		}
	}
	return catalog
}
