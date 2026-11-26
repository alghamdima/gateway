package handlers

import (
	"encoding/json"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
)

// auditVirtualKey snapshots a virtual key for the audit log without its value or any nested secret-bearing relation.
func auditVirtualKey(vk *configstoreTables.TableVirtualKey) json.RawMessage {
	if vk == nil {
		return nil
	}
	c := *vk
	c.Team = nil
	c.Customer = nil
	if vk.ProviderConfigs != nil {
		c.ProviderConfigs = make([]configstoreTables.TableVirtualKeyProviderConfig, len(vk.ProviderConfigs))
		for i, pc := range vk.ProviderConfigs {
			if pc.Keys != nil {
				keys := make([]configstoreTables.TableKey, len(pc.Keys))
				for j, k := range pc.Keys {
					keys[j] = configstoreTables.TableKey{ID: k.ID, Name: k.Name, KeyID: k.KeyID, Provider: k.Provider}
				}
				pc.Keys = keys
			}
			c.ProviderConfigs[i] = pc
		}
	}
	return lib.AuditSnapshot(c, "value")
}

// auditTeam snapshots a team for the audit log without its nested customer or virtual keys.
func auditTeam(team *configstoreTables.TableTeam) json.RawMessage {
	if team == nil {
		return nil
	}
	c := *team
	c.Customer = nil
	c.VirtualKeys = nil
	return lib.AuditSnapshot(c)
}

// auditCustomer snapshots a customer for the audit log without its virtual keys.
func auditCustomer(customer *configstoreTables.TableCustomer) json.RawMessage {
	if customer == nil {
		return nil
	}
	c := *customer
	c.VirtualKeys = nil
	if customer.Teams != nil {
		c.Teams = make([]configstoreTables.TableTeam, len(customer.Teams))
		for i, t := range customer.Teams {
			t.Customer = nil
			t.VirtualKeys = nil
			c.Teams[i] = t
		}
	}
	return lib.AuditSnapshot(c, "virtual_keys")
}

// auditProviderConfig snapshots an already-redacted provider config for the audit log, leaving keys to their own events.
func auditProviderConfig(cfg *configstore.ProviderConfig) json.RawMessage {
	if cfg == nil {
		return nil
	}
	return lib.AuditSnapshot(cfg, "keys")
}

// auditDeletedIDsLimit caps how many IDs a bulk delete puts in its audit snapshot.
const auditDeletedIDsLimit = 100

// auditDeletedIDs is the before snapshot of a bulk delete: the count, and the
// IDs themselves up to auditDeletedIDsLimit so a large delete stays small.
func auditDeletedIDs(ids []string) map[string]any {
	snap := map[string]any{"count": len(ids)}
	if len(ids) > auditDeletedIDsLimit {
		snap["ids"] = ids[:auditDeletedIDsLimit]
		snap["ids_truncated"] = true
	} else {
		snap["ids"] = ids
	}
	return snap
}

// pluginAuditSnapshot is the audit view of a plugin; Config is set only when the plugin itself redacted it.
type pluginAuditSnapshot struct {
	Name      string                   `json:"name"`
	Enabled   bool                     `json:"enabled"`
	Path      *string                  `json:"path,omitempty"`
	IsCustom  bool                     `json:"is_custom"`
	Placement *schemas.PluginPlacement `json:"placement,omitempty"`
	Order     *int                     `json:"order,omitempty"`
	Config    map[string]any           `json:"config,omitempty"`
}

// auditPlugin snapshots a plugin row, keeping its config only when the loaded plugin redacts it.
func (h *PluginsHandler) auditPlugin(plugin *configstoreTables.TablePlugin) json.RawMessage {
	if plugin == nil {
		return nil
	}
	snap := pluginAuditSnapshot{
		Name:      plugin.Name,
		Enabled:   plugin.Enabled,
		Path:      plugin.Path,
		IsCustom:  plugin.IsCustom,
		Placement: plugin.Placement,
		Order:     plugin.Order,
	}
	if configMap, ok := plugin.Config.(map[string]any); ok && h.pluginsLoader != nil {
		if redacted, err := h.pluginsLoader.ExpandPluginConfigForAPI(plugin.Name, configMap); err == nil && redacted != nil {
			snap.Config = redacted
		}
	}
	return lib.AuditSnapshot(snap)
}

// auditProxyConfig snapshots the global proxy config without its password or credentials embedded in the URL.
func auditProxyConfig(cfg *configstoreTables.GlobalProxyConfig) json.RawMessage {
	if cfg == nil {
		return nil
	}
	c := *cfg
	c.Password = ""
	if c.URL != "" {
		// RedactURLCredentials returns a URL it cannot parse unchanged, and a disabled
		// proxy is saved unvalidated, so fall back to masking the userinfo password as text.
		redacted := lib.RedactURLCredentials(c.URL)
		if redacted == c.URL {
			redacted = lib.RedactProxyURLPassword(c.URL)
		}
		c.URL = redacted
	}
	return lib.AuditSnapshot(c, "password")
}
