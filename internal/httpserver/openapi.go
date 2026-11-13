package httpserver

import (
	"encoding/json"
	"net/http"
)

// OpenAPISpec holds the OpenAPI 3.1 document as Go data.
type OpenAPISpec struct {
	OpenAPI    string                 `json:"openapi"`
	Info       InfoObject             `json:"info"`
	Servers    []ServerObject         `json:"servers,omitempty"`
	Paths      map[string]PathItem    `json:"paths"`
	Components ComponentsObject       `json:"components"`
}

type InfoObject struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type ServerObject struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type PathItem map[string]Operation

type Operation struct {
	Summary     string              `json:"summary,omitempty"`
	OperationID string              `json:"operationId,omitempty"`
	Security    []SecurityRequirement `json:"security,omitempty"`
	Parameters  []Parameter         `json:"parameters,omitempty"`
	RequestBody *RequestBody        `json:"requestBody,omitempty"`
	Responses   map[string]Response `json:"responses"`
}

type SecurityRequirement map[string][]string

type Parameter struct {
	Name        string      `json:"name"`
	In          string      `json:"in"`
	Description string      `json:"description,omitempty"`
	Required    bool        `json:"required,omitempty"`
	Schema      *Schema     `json:"schema,omitempty"`
}

type RequestBody struct {
	Description string              `json:"description,omitempty"`
	Required    bool                `json:"required,omitempty"`
	Content     map[string]MediaType `json:"content"`
}

type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

type Response struct {
	Description string              `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

type ComponentsObject struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	Schemas         map[string]*Schema        `json:"schemas,omitempty"`
}

type SecurityScheme struct {
	Type         string `json:"type"`
	Scheme       string `json:"scheme,omitempty"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Required    []string           `json:"required,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
	Ref         string             `json:"$ref,omitempty"`
}

// handleOpenAPI serves GET /api/openapi.json — public, no auth.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	spec := s.buildOpenAPISpec()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(spec)
}

// buildOpenAPISpec constructs the OpenAPI 3.1 document.
func (s *Server) buildOpenAPISpec() *OpenAPISpec {
	version := s.version
	if version == "" {
		version = "1"
	}

	spec := &OpenAPISpec{
		OpenAPI: "3.1.0",
		Info: InfoObject{
			Title:   "KelviOS API",
			Version: version,
		},
		Servers: []ServerObject{
			{URL: "/", Description: "Current host"},
		},
		Paths:      make(map[string]PathItem),
		Components: ComponentsObject{
			SecuritySchemes: map[string]SecurityScheme{
				"bearerAuth": {
					Type:         "http",
					Scheme:       "bearer",
					BearerFormat: "JWT",
				},
			},
			Schemas: make(map[string]*Schema),
		},
	}

	// Add all paths
	s.addHealthPaths(spec)
	s.addDiskPaths(spec)
	s.addPoolPaths(spec)
	s.addCollectionPaths(spec)
	s.addSharePaths(spec)
	s.addDaemonConfigPaths(spec)
	s.addAuditLogPaths(spec)
	s.addAuthPaths(spec)
	s.addVaultPaths(spec)
	s.addPowerPaths(spec)
	s.addUPSPaths(spec)
	s.addPublicSharePaths(spec)
	s.addHeatPaths(spec)
	s.addCuratorPaths(spec)
	s.addMaintenancePaths(spec)
	s.addSupplychainPaths(spec)
	s.addBackupPaths(spec)
	s.addRestoreTestPaths(spec)
	s.addShadowPromotionPaths(spec)
	s.addArrangePaths(spec)
	s.addTaggingPaths(spec)
	s.addUIPath(spec)

	return spec
}

// Helper to add a path with operation
func (s *Server) addPath(spec *OpenAPISpec, path, method string, op Operation) {
	if spec.Paths[path] == nil {
		spec.Paths[path] = make(PathItem)
	}
	spec.Paths[path][method] = op
}

// Helper for bearer auth security requirement
func (s *Server) bearerAuth() []SecurityRequirement {
	return []SecurityRequirement{{"bearerAuth": {}}}
}

// Helper for no auth
func (s *Server) noAuth() []SecurityRequirement {
	return []SecurityRequirement{{}}
}

// Helper for string schema
func (s *Server) stringSchema(desc string) *Schema {
	return &Schema{Type: "string", Description: desc}
}

// Helper for integer schema
func (s *Server) intSchema(desc string) *Schema {
	return &Schema{Type: "integer", Format: "int64", Description: desc}
}

// Helper for boolean schema
func (s *Server) boolSchema(desc string) *Schema {
	return &Schema{Type: "boolean", Description: desc}
}

// Helper for object schema with properties
func (s *Server) objectSchema(props map[string]*Schema, required []string) *Schema {
	return &Schema{Type: "object", Properties: props, Required: required}
}

// Helper for array schema
func (s *Server) arraySchema(items *Schema) *Schema {
	return &Schema{Type: "array", Items: items}
}

// Helper for path parameter
func (s *Server) pathParam(name, desc string) Parameter {
	return Parameter{Name: name, In: "path", Description: desc, Required: true, Schema: s.stringSchema(desc)}
}

// Helper for query parameter
func (s *Server) queryParam(name, desc string, required bool) Parameter {
	return Parameter{Name: name, In: "query", Description: desc, Required: required, Schema: s.stringSchema(desc)}
}

// Helper for integer query parameter
func (s *Server) intQueryParam(name, desc string, required bool) Parameter {
	return Parameter{Name: name, In: "query", Description: desc, Required: required, Schema: s.intSchema(desc)}
}

// Helper for standard responses
func (s *Server) stdResponses(auth bool) map[string]Response {
	resp := map[string]Response{
		"200": {Description: "OK"},
		"400": {Description: "Bad Request"},
		"401": {Description: "Unauthorized"},
		"404": {Description: "Not Found"},
		"405": {Description: "Method Not Allowed"},
		"500": {Description: "Internal Server Error"},
	}
	if !auth {
		delete(resp, "401")
	}
	return resp
}

// Helper for write responses (201/204)
func (s *Server) writeResponses(auth bool) map[string]Response {
	resp := map[string]Response{
		"201": {Description: "Created"},
		"204": {Description: "No Content"},
		"400": {Description: "Bad Request"},
		"401": {Description: "Unauthorized"},
		"404": {Description: "Not Found"},
		"405": {Description: "Method Not Allowed"},
		"409": {Description: "Conflict"},
		"500": {Description: "Internal Server Error"},
	}
	if !auth {
		delete(resp, "401")
	}
	return resp
}

// Health paths
func (s *Server) addHealthPaths(spec *OpenAPISpec) {
	s.addPath(spec, "/healthz", "get", Operation{
		Summary:     "Health check",
		OperationID: "healthz",
		Security:    s.noAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"status":         s.stringSchema("Status (ok)"),
					"version":        s.stringSchema("Server version"),
					"uptime_seconds": s.intSchema("Uptime in seconds"),
				}, []string{})},
			}},
		},
	})
	s.addPath(spec, "/api/openapi.json", "get", Operation{
		Summary:     "OpenAPI document",
		OperationID: "getOpenAPI",
		Security:    s.noAuth(),
		Responses: map[string]Response{
			"200": {Description: "OpenAPI 3.1 document", Content: map[string]MediaType{
				"application/json": {Schema: &Schema{Type: "object"}},
			}},
		},
	})
}

// Disk paths
func (s *Server) addDiskPaths(spec *OpenAPISpec) {
	diskItem := s.objectSchema(map[string]*Schema{
		"name":        s.stringSchema("Disk name"),
		"size_bytes":  s.intSchema("Size in bytes"),
		"model":       s.stringSchema("Model"),
		"serial":      s.stringSchema("Serial"),
		"type":        s.stringSchema("Type (HDD/SSD/NVMe)"),
		"pool_id":     s.stringSchema("Pool ID if assigned"),
	}, []string{})
	s.addPath(spec, "/api/disks", "get", Operation{
		Summary:     "List disks",
		OperationID: "listDisks",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disks": s.arraySchema(diskItem),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Pool paths
func (s *Server) addPoolPaths(spec *OpenAPISpec) {
	poolItem := s.objectSchema(map[string]*Schema{
		"id":              s.stringSchema("Pool ID"),
		"name":            s.stringSchema("Pool name"),
		"node_id":         s.stringSchema("Node ID"),
		"tier":            s.stringSchema("Tier (hot/warm/cold)"),
		"mechanism":       s.stringSchema("Mechanism"),
		"driver":          s.stringSchema("Driver"),
		"disk_names":      s.arraySchema(s.stringSchema("Disk name")),
		"mountpoint":      s.stringSchema("Mountpoint"),
		"size_bytes":      s.intSchema("Size in bytes"),
		"used_bytes":      s.intSchema("Used bytes"),
		"encrypted":       s.boolSchema("Whether encrypted"),
		"encryption_mode": s.stringSchema("Encryption mode"),
		"status":          s.stringSchema("Status"),
		"created_at":      s.intSchema("Created at (unix)"),
		"updated_at":      s.intSchema("Updated at (unix)"),
	}, []string{})

	createPoolReq := s.objectSchema(map[string]*Schema{
		"name":                   s.stringSchema("Pool name"),
		"tier":                   s.stringSchema("Tier (hot/warm/cold)"),
		"mechanism":              s.stringSchema("Mechanism (single/mirror/parity-scheduled/stripe/zfs)"),
		"disks":                  s.arraySchema(s.stringSchema("Disk name")),
		"mountpoint":             s.stringSchema("Mountpoint"),
		"encrypted":              s.boolSchema("Whether encrypted (legacy)"),
		"confirm_stripe":         s.boolSchema("Confirm stripe (required for stripe mechanism)"),
		"encryption_mode":        s.stringSchema("Encryption mode (none/manual/tpm2)"),
		"recovery_key_created":   s.boolSchema("Recovery key created (required for manual/tpm2)"),
	}, []string{"name", "tier", "mechanism", "disks"})

	// GET /api/pools
	s.addPath(spec, "/api/pools", "get", Operation{
		Summary:     "List pools",
		OperationID: "listPools",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"pools": s.arraySchema(poolItem),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/pools
	s.addPath(spec, "/api/pools", "post", Operation{
		Summary:     "Create pool",
		OperationID: "createPool",
		Security:    s.bearerAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createPoolReq},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/pools/plan
	s.addPath(spec, "/api/pools/plan", "get", Operation{
		Summary:     "Plan pool creation",
		OperationID: "planPool",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.queryParam("tier", "Tier (hot/warm/cold)", true),
			s.queryParam("mechanism", "Mechanism", true),
			s.queryParam("disks", "Comma-separated disk names", true),
			s.queryParam("encrypted", "Whether encrypted", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/pools/{id}
	s.addPath(spec, "/api/pools/{id}", "get", Operation{
		Summary:     "Get pool by ID",
		OperationID: "getPool",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"pool": poolItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/pools/{id}
	s.addPath(spec, "/api/pools/{id}", "delete", Operation{
		Summary:     "Delete pool",
		OperationID: "deletePool",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"405": {Description: "Method Not Allowed"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/pools/{id}/collections
	s.addPath(spec, "/api/pools/{id}/collections", "get", Operation{
		Summary:     "List collections in pool",
		OperationID: "listCollections",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"collections": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":               s.stringSchema("Collection ID"),
						"name":             s.stringSchema("Collection name"),
						"pool_id":          s.stringSchema("Pool ID"),
						"path":             s.stringSchema("Path"),
						"disk_name":        s.stringSchema("Disk name"),
						"size_bytes":       s.intSchema("Size in bytes"),
						"last_accessed_at": s.intSchema("Last accessed at (unix)"),
						"created_at":       s.intSchema("Created at (unix)"),
						"updated_at":       s.intSchema("Updated at (unix)"),
					}, []string{})),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/pools/{id}/collections
	createCollReq := s.objectSchema(map[string]*Schema{
		"name":      s.stringSchema("Collection name"),
		"path":      s.stringSchema("Path"),
		"disk_name": s.stringSchema("Disk name (optional)"),
	}, []string{"name", "path"})
	s.addPath(spec, "/api/pools/{id}/collections", "post", Operation{
		Summary:     "Create collection in pool",
		OperationID: "createCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createCollReq},
		}},
		Responses: s.writeResponses(true),
	})
	// POST /api/pools/{id}/disk-replace/plan
	s.addPath(spec, "/api/pools/{id}/disk-replace/plan", "post", Operation{
		Summary:     "Plan disk replacement",
		OperationID: "planDiskReplace",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: s.objectSchema(map[string]*Schema{
				"old_disk": s.stringSchema("Old disk name"),
				"new_disk": s.stringSchema("New disk name"),
			}, []string{"old_disk", "new_disk"})},
		}},
		Responses: s.writeResponses(true),
	})
	// POST /api/pools/{id}/disk-expand/plan
	s.addPath(spec, "/api/pools/{id}/disk-expand/plan", "post", Operation{
		Summary:     "Plan disk expansion",
		OperationID: "planDiskExpand",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: s.objectSchema(map[string]*Schema{
				"new_disk": s.stringSchema("New disk name"),
			}, []string{"new_disk"})},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/pools/{id}/disk-operations
	s.addPath(spec, "/api/pools/{id}/disk-operations", "get", Operation{
		Summary:     "List disk operations",
		OperationID: "listDiskOperations",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: s.stdResponses(true),
	})
	// GET /api/pools/{id}/placement
	s.addPath(spec, "/api/pools/{id}/placement", "get", Operation{
		Summary:     "Get pool placement info",
		OperationID: "getPoolPlacement",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: s.stdResponses(true),
	})
	// GET /api/pools/{id}/tier-policy
	s.addPath(spec, "/api/pools/{id}/tier-policy", "get", Operation{
		Summary:     "Get tier policy",
		OperationID: "getTierPolicy",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"pool_id":              s.stringSchema("Pool ID"),
					"mode":                 s.stringSchema("Mode"),
					"cold_threshold_days":  s.intSchema("Cold threshold days"),
					"auto_move_wait_days":  s.intSchema("Auto move wait days"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// PUT /api/pools/{id}/tier-policy
	tierPolicyReq := s.objectSchema(map[string]*Schema{
		"mode":                 s.stringSchema("Mode"),
		"cold_threshold_days":  s.intSchema("Cold threshold days"),
		"auto_move_wait_days":  s.intSchema("Auto move wait days"),
	}, []string{"mode", "cold_threshold_days", "auto_move_wait_days"})
	s.addPath(spec, "/api/pools/{id}/tier-policy", "put", Operation{
		Summary:     "Set tier policy",
		OperationID: "setTierPolicy",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Pool ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: tierPolicyReq},
		}},
		Responses: s.writeResponses(true),
	})
}

// Collection paths
func (s *Server) addCollectionPaths(spec *OpenAPISpec) {
	collItem := s.objectSchema(map[string]*Schema{
		"id":               s.stringSchema("Collection ID"),
		"name":             s.stringSchema("Collection name"),
		"pool_id":          s.stringSchema("Pool ID"),
		"path":             s.stringSchema("Path"),
		"disk_name":        s.stringSchema("Disk name"),
		"size_bytes":       s.intSchema("Size in bytes"),
		"last_accessed_at": s.intSchema("Last accessed at (unix)"),
		"created_at":       s.intSchema("Created at (unix)"),
		"updated_at":       s.intSchema("Updated at (unix)"),
	}, []string{})

	// GET /api/collections/{id}
	s.addPath(spec, "/api/collections/{id}", "get", Operation{
		Summary:     "Get collection by ID",
		OperationID: "getCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"collection": collItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/collections/{id}
	s.addPath(spec, "/api/collections/{id}", "delete", Operation{
		Summary:     "Delete collection",
		OperationID: "deleteCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/shares
	s.addPath(spec, "/api/collections/{id}/shares", "get", Operation{
		Summary:     "List collection shares",
		OperationID: "listCollectionShares",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"shares": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":            s.stringSchema("Share ID"),
						"collection_id": s.stringSchema("Collection ID"),
						"name":          s.stringSchema("Share name"),
						"protocol":      s.stringSchema("Protocol (smb/nfs/sftp)"),
						"read_only":     s.boolSchema("Read-only"),
						"metadata":      s.stringSchema("Metadata"),
						"created_at":    s.intSchema("Created at (unix)"),
						"updated_at":    s.intSchema("Updated at (unix)"),
					}, []string{})),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/shares
	createShareReq := s.objectSchema(map[string]*Schema{
		"name":      s.stringSchema("Share name"),
		"protocol":  s.stringSchema("Protocol (smb/nfs/sftp)"),
		"read_only": s.boolSchema("Read-only"),
		"metadata":  s.stringSchema("Metadata (optional)"),
	}, []string{"name", "protocol"})
	s.addPath(spec, "/api/collections/{id}/shares", "post", Operation{
		Summary:     "Create collection share",
		OperationID: "createCollectionShare",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createShareReq},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/collections/{id}/share-links
	s.addPath(spec, "/api/collections/{id}/share-links", "get", Operation{
		Summary:     "List share links",
		OperationID: "listShareLinks",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"share_links": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":           s.stringSchema("Link ID"),
						"rel_path":     s.stringSchema("Relative path"),
						"expires_at":   s.intSchema("Expires at (unix, optional)"),
						"has_password": s.boolSchema("Has password"),
						"revoked_at":   s.intSchema("Revoked at (unix, optional)"),
						"created_by":   s.stringSchema("Created by"),
						"created_at":   s.intSchema("Created at (unix)"),
					}, []string{})),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/share-links
	createLinkReq := s.objectSchema(map[string]*Schema{
		"rel_path":   s.stringSchema("Relative path"),
		"expires_at": s.intSchema("Expires at (unix, optional)"),
		"password":   s.stringSchema("Password (optional)"),
	}, []string{"rel_path"})
	s.addPath(spec, "/api/collections/{id}/share-links", "post", Operation{
		Summary:     "Create share link",
		OperationID: "createShareLink",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createLinkReq},
		}},
		Responses: s.writeResponses(true),
	})
	// DELETE /api/collections/{id}/share-links/{linkId}
	s.addPath(spec, "/api/collections/{id}/share-links/{linkId}", "delete", Operation{
		Summary:     "Revoke share link",
		OperationID: "revokeShareLink",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.pathParam("linkId", "Link ID"),
		},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/scan
	s.addPath(spec, "/api/collections/{id}/scan", "post", Operation{
		Summary:     "Scan collection files",
		OperationID: "scanCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"added":   s.intSchema("Files added"),
					"updated": s.intSchema("Files updated"),
					"removed": s.intSchema("Files removed"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (root missing)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/imports
	s.addPath(spec, "/api/collections/{id}/imports", "get", Operation{
		Summary:     "List collection imports",
		OperationID: "listCollectionImports",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.intQueryParam("limit", "Limit", false),
			s.intQueryParam("offset", "Offset", false),
		},
		Responses: s.stdResponses(true),
	})
	// POST /api/collections/{id}/import
	s.addPath(spec, "/api/collections/{id}/import", "post", Operation{
		Summary:     "Import into collection",
		OperationID: "importCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: s.objectSchema(map[string]*Schema{
				"source": s.stringSchema("Source path"),
			}, []string{"source"})},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/collections/{id}/files/preview
	s.addPath(spec, "/api/collections/{id}/files/preview", "get", Operation{
		Summary:     "Preview collection file",
		OperationID: "previewCollectionFile",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative path", true),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"collection_id":     s.stringSchema("Collection ID"),
					"rel_path":          s.stringSchema("Relative path"),
					"kind":              s.stringSchema("Kind (image/audio/video/other)"),
					"mime":              s.stringSchema("MIME type"),
					"size_bytes":        s.intSchema("Size in bytes"),
					"width":             s.intSchema("Width (images)"),
					"height":            s.intSchema("Height (images)"),
					"duration_seconds":  s.intSchema("Duration (media)"),
					"preview_available": s.boolSchema("Preview available"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (cold tier)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/files/content
	s.addPath(spec, "/api/collections/{id}/files/content", "get", Operation{
		Summary:     "Download collection file",
		OperationID: "downloadCollectionFile",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative path", true),
		},
		Responses: map[string]Response{
			"200": {Description: "File content"},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (cold tier, wake_required)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/files/content
	s.addPath(spec, "/api/collections/{id}/files/content", "post", Operation{
		Summary:     "Upload collection file",
		OperationID: "uploadCollectionFile",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative path", true),
			s.queryParam("overwrite", "Overwrite if exists (true/false)", false),
		},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/octet-stream": {Schema: &Schema{Type: "string", Format: "binary"}},
		}},
		Responses: s.writeResponses(true),
	})
	// DELETE /api/collections/{id}/files/content
	s.addPath(spec, "/api/collections/{id}/files/content", "delete", Operation{
		Summary:     "Trash collection file",
		OperationID: "deleteCollectionFile",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative path", true),
			s.queryParam("dir", "Directory delete (true/false)", false),
		},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (cold tier)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/files/mkdir
	s.addPath(spec, "/api/collections/{id}/files/mkdir", "post", Operation{
		Summary:     "Create directory",
		OperationID: "mkdirCollectionFile",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative directory path", true),
		},
		Responses: s.writeResponses(true),
	})
	// POST /api/collections/{id}/files/move
	moveReq := s.objectSchema(map[string]*Schema{
		"from": s.stringSchema("Source relative path"),
		"to":   s.stringSchema("Destination relative path"),
	}, []string{"from", "to"})
	s.addPath(spec, "/api/collections/{id}/files/move", "post", Operation{
		Summary:     "Move collection file",
		OperationID: "moveCollectionFile",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: moveReq},
		}},
		Responses: s.writeResponses(true),
	})
	// POST /api/collections/{id}/files/copy
	s.addPath(spec, "/api/collections/{id}/files/copy", "post", Operation{
		Summary:     "Copy collection file",
		OperationID: "copyCollectionFile",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: moveReq},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/collections/{id}/files/heat
	s.addPath(spec, "/api/collections/{id}/files/heat", "get", Operation{
		Summary:     "Get file heat",
		OperationID: "getFileHeat",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Relative path", true),
			s.intQueryParam("window_days", "Window days (default 30)", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"count":       s.intSchema("Access count"),
					"last_access": s.intSchema("Last access (unix)"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/files/heated
	s.addPath(spec, "/api/collections/{id}/files/heated", "get", Operation{
		Summary:     "Get top heated files",
		OperationID: "getTopHeated",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.intQueryParam("limit", "Limit (default 50, max 500)", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"files": s.arraySchema(s.objectSchema(map[string]*Schema{
						"rel_path":    s.stringSchema("Relative path"),
						"count":       s.intSchema("Access count"),
						"last_access": s.intSchema("Last access (unix)"),
					}, []string{})),
					"total": s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/heat
	s.addPath(spec, "/api/collections/{id}/heat", "get", Operation{
		Summary:     "Get collection heat",
		OperationID: "getCollectionHeat",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"collection_id": s.stringSchema("Collection ID"),
					"score":         s.intSchema("Heat score"),
					"event_count":   s.intSchema("Event count"),
					"last_access":   s.intSchema("Last access (unix)"),
					"size_bytes":    s.intSchema("Size in bytes"),
					"computed_at":   s.intSchema("Computed at (unix)"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/pin
	s.addPath(spec, "/api/collections/{id}/pin", "post", Operation{
		Summary:     "Pin collection",
		OperationID: "pinCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		RequestBody: &RequestBody{Required: false, Content: map[string]MediaType{
			"application/json": {Schema: s.objectSchema(map[string]*Schema{
				"note": s.stringSchema("Note (optional)"),
			}, []string{})},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/collections/{id}/pin
	s.addPath(spec, "/api/collections/{id}/pin", "delete", Operation{
		Summary:     "Unpin collection",
		OperationID: "unpinCollection",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/trash
	s.addPath(spec, "/api/collections/{id}/trash", "get", Operation{
		Summary:     "List trash items",
		OperationID: "listTrash",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.intQueryParam("limit", "Limit (default 100)", false),
			s.intQueryParam("offset", "Offset (default 0)", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"entries": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":                s.stringSchema("Trash ID"),
						"collection_id":     s.stringSchema("Collection ID"),
						"original_rel_path": s.stringSchema("Original relative path"),
						"trash_rel_path":    s.stringSchema("Trash relative path"),
						"kind":              s.stringSchema("Kind"),
						"size_bytes":        s.intSchema("Size in bytes"),
						"disk_name":         s.stringSchema("Disk name"),
						"deleted_at":        s.intSchema("Deleted at (unix)"),
						"retention_days":    s.intSchema("Retention days"),
						"actor":             s.stringSchema("Actor"),
					}, []string{})),
					"total": s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/collections/{id}/trash
	s.addPath(spec, "/api/collections/{id}/trash", "delete", Operation{
		Summary:     "Empty trash",
		OperationID: "emptyTrash",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/collections/{id}/trash/{trashId}/restore
	restoreReq := s.objectSchema(map[string]*Schema{
		"target_rel_path": s.stringSchema("Target relative path (optional, defaults to original)"),
	}, []string{})
	s.addPath(spec, "/api/collections/{id}/trash/{trashId}/restore", "post", Operation{
		Summary:     "Restore trash item",
		OperationID: "restoreTrashItem",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.pathParam("trashId", "Trash ID"),
		},
		RequestBody: &RequestBody{Required: false, Content: map[string]MediaType{
			"application/json": {Schema: restoreReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (target exists or cold tier)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/collections/{id}/trash/{trashId}
	s.addPath(spec, "/api/collections/{id}/trash/{trashId}", "delete", Operation{
		Summary:     "Permanently delete trash item",
		OperationID: "deleteTrashItem",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.pathParam("trashId", "Trash ID"),
		},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/collections/{id}/files
	s.addPath(spec, "/api/collections/{id}/files", "get", Operation{
		Summary:     "Browse collection files",
		OperationID: "browseCollectionFiles",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("id", "Collection ID"),
			s.queryParam("path", "Path prefix", false),
			s.intQueryParam("limit", "Limit", false),
			s.intQueryParam("offset", "Offset", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"entries": s.arraySchema(s.objectSchema(map[string]*Schema{
						"rel_path": s.stringSchema("Relative path"),
						"kind":     s.stringSchema("Kind (file/dir)"),
						"size":     s.intSchema("Size in bytes"),
						"offline":  s.boolSchema("Offline (cold tier)"),
						"shadowed": s.boolSchema("Has shadow"),
						"dirty":    s.boolSchema("Shadow is dirty"),
					}, []string{})),
					"tier": s.stringSchema("Pool tier"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Share paths
func (s *Server) addSharePaths(spec *OpenAPISpec) {
	shareItem := s.objectSchema(map[string]*Schema{
		"id":            s.stringSchema("Share ID"),
		"collection_id": s.stringSchema("Collection ID"),
		"name":          s.stringSchema("Share name"),
		"protocol":      s.stringSchema("Protocol (smb/nfs/sftp)"),
		"read_only":     s.boolSchema("Read-only"),
		"metadata":      s.stringSchema("Metadata"),
		"created_at":    s.intSchema("Created at (unix)"),
		"updated_at":    s.intSchema("Updated at (unix)"),
	}, []string{})

	// GET /api/shares/{id}
	s.addPath(spec, "/api/shares/{id}", "get", Operation{
		Summary:     "Get share by ID",
		OperationID: "getShare",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Share ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"share": shareItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/shares/{id}
	s.addPath(spec, "/api/shares/{id}", "delete", Operation{
		Summary:     "Delete share",
		OperationID: "deleteShare",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Share ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/shares/{id}/daemon-config
	s.addPath(spec, "/api/shares/{id}/daemon-config", "get", Operation{
		Summary:     "Get daemon config for share",
		OperationID: "getDaemonConfigShare",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Share ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"protocol":    s.stringSchema("Protocol"),
					"config_text": s.stringSchema("Config text"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Daemon config paths
func (s *Server) addDaemonConfigPaths(spec *OpenAPISpec) {
	// GET /api/daemon-config
	s.addPath(spec, "/api/daemon-config", "get", Operation{
		Summary:     "Get all daemon configs",
		OperationID: "getDaemonConfigAll",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"configs": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":             s.stringSchema("Share ID"),
						"name":           s.stringSchema("Share name"),
						"protocol":       s.stringSchema("Protocol"),
						"read_only":      s.boolSchema("Read-only"),
						"metadata":       s.stringSchema("Metadata"),
						"collection_id":  s.stringSchema("Collection ID"),
						"collection_path": s.stringSchema("Collection path"),
						"pool_mountpoint": s.stringSchema("Pool mountpoint"),
						"pool_name":      s.stringSchema("Pool name"),
					}, []string{})),
					"errors": s.arraySchema(s.stringSchema("Error message")),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Audit log paths
func (s *Server) addAuditLogPaths(spec *OpenAPISpec) {
	// GET /api/audit-log
	s.addPath(spec, "/api/audit-log", "get", Operation{
		Summary:     "List audit log entries",
		OperationID: "listAuditLog",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.intQueryParam("limit", "Limit (default 100)", false),
			s.intQueryParam("offset", "Offset (default 0)", false),
			s.queryParam("action", "Action filter", false),
			s.queryParam("target_type", "Target type filter", false),
			s.queryParam("result", "Result filter", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"entries": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":          s.stringSchema("Entry ID"),
						"created_at":  s.intSchema("Created at (unix)"),
						"actor":       s.stringSchema("Actor"),
						"action":      s.stringSchema("Action"),
						"target_type": s.stringSchema("Target type"),
						"target_id":   s.stringSchema("Target ID"),
						"result":      s.stringSchema("Result"),
						"detail":      s.stringSchema("Detail (optional)"),
					}, []string{})),
					"total": s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Auth paths
func (s *Server) addAuthPaths(spec *OpenAPISpec) {
	setupReq := s.objectSchema(map[string]*Schema{
		"username": s.stringSchema("Username"),
		"password": s.stringSchema("Password (min 12 chars)"),
	}, []string{"username", "password"})
	loginReq := s.objectSchema(map[string]*Schema{
		"username": s.stringSchema("Username"),
		"password": s.stringSchema("Password"),
	}, []string{"username", "password"})

	// POST /api/auth/setup
	s.addPath(spec, "/api/auth/setup", "post", Operation{
		Summary:     "One-time admin setup",
		OperationID: "authSetup",
		Security:    s.noAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: setupReq},
		}},
		Responses: map[string]Response{
			"201": {Description: "Created", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"username": s.stringSchema("Username"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"409": {Description: "Conflict (setup closed)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/auth/login
	s.addPath(spec, "/api/auth/login", "post", Operation{
		Summary:     "Login",
		OperationID: "authLogin",
		Security:    s.noAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: loginReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"token":       s.stringSchema("Bearer token"),
					"expires_at":  s.intSchema("Expires at (unix)"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"423": {Description: "Locked"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/auth/me
	s.addPath(spec, "/api/auth/me", "get", Operation{
		Summary:     "Get current user",
		OperationID: "authMe",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"username": s.stringSchema("Username"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/auth/logout
	s.addPath(spec, "/api/auth/logout", "post", Operation{
		Summary:     "Logout",
		OperationID: "authLogout",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Vault paths
func (s *Server) addVaultPaths(spec *OpenAPISpec) {
	passphraseReq := s.objectSchema(map[string]*Schema{
		"passphrase": s.stringSchema("Passphrase (min 8 chars)"),
	}, []string{"passphrase"})
	secretReq := s.objectSchema(map[string]*Schema{
		"value": s.stringSchema("Secret value"),
	}, []string{"value"})

	// POST /api/vault/setup
	s.addPath(spec, "/api/vault/setup", "post", Operation{
		Summary:     "Setup vault",
		OperationID: "vaultSetup",
		Security:    s.bearerAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: passphraseReq},
		}},
		Responses: map[string]Response{
			"201": {Description: "Created", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"exists":   s.boolSchema("Vault exists"),
					"unlocked": s.boolSchema("Vault unlocked"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"409": {Description: "Conflict (vault exists)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/vault/unlock
	s.addPath(spec, "/api/vault/unlock", "post", Operation{
		Summary:     "Unlock vault",
		OperationID: "vaultUnlock",
		Security:    s.bearerAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: passphraseReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"exists":   s.boolSchema("Vault exists"),
					"unlocked": s.boolSchema("Vault unlocked"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/vault/lock
	s.addPath(spec, "/api/vault/lock", "post", Operation{
		Summary:     "Lock vault",
		OperationID: "vaultLock",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/vault/status
	s.addPath(spec, "/api/vault/status", "get", Operation{
		Summary:     "Get vault status",
		OperationID: "vaultStatus",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"exists":   s.boolSchema("Vault exists"),
					"unlocked": s.boolSchema("Vault unlocked"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/secrets
	s.addPath(spec, "/api/secrets", "get", Operation{
		Summary:     "List secrets",
		OperationID: "listSecrets",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"secrets": s.arraySchema(s.objectSchema(map[string]*Schema{
						"name":       s.stringSchema("Secret name"),
						"created_at": s.intSchema("Created at (unix)"),
						"updated_at": s.intSchema("Updated at (unix)"),
					}, []string{})),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"423": {Description: "Locked"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// PUT /api/secrets/{name}
	s.addPath(spec, "/api/secrets/{name}", "put", Operation{
		Summary:     "Store secret",
		OperationID: "putSecret",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("name", "Secret name")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: secretReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"name": s.stringSchema("Secret name"),
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"423": {Description: "Locked"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/secrets/{name}
	s.addPath(spec, "/api/secrets/{name}", "delete", Operation{
		Summary:     "Delete secret",
		OperationID: "deleteSecret",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("name", "Secret name")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"423": {Description: "Locked"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Power paths
func (s *Server) addPowerPaths(spec *OpenAPISpec) {
	powerItem := s.objectSchema(map[string]*Schema{
		"disk":             s.stringSchema("Disk name"),
		"pool_id":          s.stringSchema("Pool ID"),
		"pool_name":        s.stringSchema("Pool name"),
		"tier":             s.stringSchema("Tier (hot/warm/cold, null if unpooled)"),
		"power_state":      s.stringSchema("Power state (unknown/active/standby/off)"),
		"last_activity":    s.intSchema("Last activity (unix)"),
		"keep_awake_until": s.intSchema("Keep awake until (unix)"),
		"window_minutes":   s.intSchema("Window minutes"),
	}, []string{})

	scheduleItem := s.objectSchema(map[string]*Schema{
		"id":         s.stringSchema("Schedule ID"),
		"disk":       s.stringSchema("Disk name"),
		"days":       s.stringSchema("Days (comma-separated Mon-Sun)"),
		"start_min":  s.intSchema("Start minute (0-1439)"),
		"end_min":    s.intSchema("End minute (0-1439)"),
		"enabled":    s.boolSchema("Enabled"),
		"created_at": s.intSchema("Created at (unix)"),
		"updated_at": s.intSchema("Updated at (unix)"),
	}, []string{})

	wakeChecklistRun := s.objectSchema(map[string]*Schema{
		"id":            s.stringSchema("Run ID"),
		"pool_id":       s.stringSchema("Pool ID"),
		"disk":          s.stringSchema("Disk name"),
		"started_at":    s.intSchema("Started at (unix)"),
		"finished_at":   s.intSchema("Finished at (unix)"),
		"steps":         s.arraySchema(s.objectSchema(map[string]*Schema{
			"step":   s.stringSchema("Step name"),
			"status": s.stringSchema("Status"),
			"detail": s.stringSchema("Detail"),
		}, []string{})),
	}, []string{})

	// GET /api/power
	s.addPath(spec, "/api/power", "get", Operation{
		Summary:     "List power states",
		OperationID: "listPower",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disks": s.arraySchema(powerItem),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/power/{disk}
	s.addPath(spec, "/api/power/{disk}", "get", Operation{
		Summary:     "Get power state for disk",
		OperationID: "getPower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/wake
	s.addPath(spec, "/api/power/{disk}/wake", "post", Operation{
		Summary:     "Wake disk",
		OperationID: "wakePower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/sleep
	s.addPath(spec, "/api/power/{disk}/sleep", "post", Operation{
		Summary:     "Sleep disk",
		OperationID: "sleepPower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (hot tier or unpooled)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/activity
	s.addPath(spec, "/api/power/{disk}/activity", "post", Operation{
		Summary:     "Record activity",
		OperationID: "activityPower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/stay-awake
	stayAwakeReq := s.objectSchema(map[string]*Schema{
		"until": s.intSchema("Keep awake until (unix, must be future)"),
	}, []string{"until"})
	s.addPath(spec, "/api/power/{disk}/stay-awake", "post", Operation{
		Summary:     "Set stay awake",
		OperationID: "stayAwakePower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: stayAwakeReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"400": {Description: "Bad Request (past until)"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/window
	windowReq := s.objectSchema(map[string]*Schema{
		"minutes": s.intSchema("Window minutes (15-240)"),
	}, []string{"minutes"})
	s.addPath(spec, "/api/power/{disk}/window", "post", Operation{
		Summary:     "Set window",
		OperationID: "windowPower",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: windowReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"disk": powerItem,
				}, []string{})},
			}},
			"400": {Description: "Bad Request (out of range)"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (unpooled)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/power/{disk}/schedules
	s.addPath(spec, "/api/power/{disk}/schedules", "get", Operation{
		Summary:     "List power schedules",
		OperationID: "listPowerSchedules",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"schedules": s.arraySchema(scheduleItem),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/schedules
	createSchedReq := s.objectSchema(map[string]*Schema{
		"days":      s.stringSchema("Days (comma-separated Mon-Sun)"),
		"start_min": s.intSchema("Start minute (0-1439)"),
		"end_min":   s.intSchema("End minute (0-1439)"),
	}, []string{"days", "start_min", "end_min"})
	s.addPath(spec, "/api/power/{disk}/schedules", "post", Operation{
		Summary:     "Create power schedule",
		OperationID: "createPowerSchedule",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createSchedReq},
		}},
		Responses: map[string]Response{
			"201": {Description: "Created", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"schedule": scheduleItem,
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (duplicate)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/power/{disk}/schedules/{id}
	s.addPath(spec, "/api/power/{disk}/schedules/{id}", "delete", Operation{
		Summary:     "Delete power schedule",
		OperationID: "deletePowerSchedule",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("disk", "Disk name"),
			s.pathParam("id", "Schedule ID"),
		},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// PATCH /api/power/{disk}/schedules/{id}
	patchSchedReq := s.objectSchema(map[string]*Schema{
		"enabled": s.boolSchema("Enabled"),
	}, []string{"enabled"})
	s.addPath(spec, "/api/power/{disk}/schedules/{id}", "patch", Operation{
		Summary:     "Update power schedule",
		OperationID: "patchPowerSchedule",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("disk", "Disk name"),
			s.pathParam("id", "Schedule ID"),
		},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: patchSchedReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"schedule": scheduleItem,
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/power/{disk}/wake-checklist
	s.addPath(spec, "/api/power/{disk}/wake-checklist", "get", Operation{
		Summary:     "List wake checklist runs",
		OperationID: "listWakeChecklist",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.pathParam("disk", "Disk name"),
			s.intQueryParam("limit", "Limit (default 50, max 500)", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"runs": s.arraySchema(wakeChecklistRun),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/power/{disk}/wake-checklist/run
	s.addPath(spec, "/api/power/{disk}/wake-checklist/run", "post", Operation{
		Summary:     "Run wake checklist",
		OperationID: "runWakeChecklist",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("disk", "Disk name")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"run": wakeChecklistRun,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (not cold pool)"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// UPS paths
func (s *Server) addUPSPaths(spec *OpenAPISpec) {
	readingItem := s.objectSchema(map[string]*Schema{
		"present":         s.boolSchema("Present"),
		"state":           s.stringSchema("State"),
		"charge_percent":  s.intSchema("Charge percent"),
		"runtime_seconds": s.intSchema("Runtime seconds"),
	}, []string{})
	decisionItem := s.objectSchema(map[string]*Schema{
		"event":           s.stringSchema("Event"),
		"begin_shutdown":  s.boolSchema("Begin shutdown"),
		"shutdown_reason": s.stringSchema("Shutdown reason"),
	}, []string{})

	// POST /api/ups/reading
	s.addPath(spec, "/api/ups/reading", "post", Operation{
		Summary:     "Submit UPS reading",
		OperationID: "upsReading",
		Security:    s.bearerAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: &Schema{Type: "object"}},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"reading":  readingItem,
					"decision": decisionItem,
				}, []string{})},
			}},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/ups/status
	s.addPath(spec, "/api/ups/status", "get", Operation{
		Summary:     "Get UPS status",
		OperationID: "upsStatus",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"reading":             readingItem,
					"critical_percent":    s.intSchema("Critical percent"),
					"shutdown_initiated":  s.boolSchema("Shutdown initiated"),
					"last_event":          s.stringSchema("Last event"),
					"poller_enabled":      s.boolSchema("Poller enabled"),
					"last_poll_error":     s.stringSchema("Last poll error"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/ups/events
	s.addPath(spec, "/api/ups/events", "get", Operation{
		Summary:     "List UPS events",
		OperationID: "upsEvents",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.intQueryParam("limit", "Limit (default 100)", false),
			s.intQueryParam("offset", "Offset (default 0)", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"events": s.arraySchema(s.objectSchema(map[string]*Schema{
						"id":             s.stringSchema("Event ID"),
						"ts":             s.intSchema("Timestamp (unix)"),
						"kind":           s.stringSchema("Event kind"),
						"state":          s.stringSchema("State"),
						"charge_percent": s.intSchema("Charge percent"),
						"detail":         s.stringSchema("Detail"),
					}, []string{})),
					"total": s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Public share paths
func (s *Server) addPublicSharePaths(spec *OpenAPISpec) {
	// GET /api/public/share/{token}
	s.addPath(spec, "/api/public/share/{token}", "get", Operation{
		Summary:     "Access public share",
		OperationID: "publicShareAccess",
		Security:    s.noAuth(),
		Parameters:  []Parameter{s.pathParam("token", "Share token")},
		Responses: map[string]Response{
			"200": {Description: "File content"},
			"400": {Description: "Bad Request"},
			"401": {Description: "Unauthorized (password required)"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (cold tier, wake_required)"},
			"410": {Description: "Gone (revoked/expired)"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Heat paths
func (s *Server) addHeatPaths(spec *OpenAPISpec) {
	// POST /api/heat/recompute
	s.addPath(spec, "/api/heat/recompute", "post", Operation{
		Summary:     "Recompute heat scores",
		OperationID: "heatRecompute",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"computed": s.intSchema("Number of collections computed"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Curator paths
func (s *Server) addCuratorPaths(spec *OpenAPISpec) {
	recItem := s.objectSchema(map[string]*Schema{
		"id":             s.stringSchema("Recommendation ID"),
		"collection_id":  s.stringSchema("Collection ID"),
		"pool_id":        s.stringSchema("Pool ID"),
		"direction":      s.stringSchema("Direction (to_cold/to_warm/to_hot)"),
		"reason":         s.stringSchema("Reason"),
		"size_bytes":     s.intSchema("Size in bytes"),
		"last_access":    s.intSchema("Last access (unix)"),
		"score":          s.intSchema("Heat score"),
		"status":         s.stringSchema("Status (pending/approved/rejected)"),
		"created_at":     s.intSchema("Created at (unix)"),
		"decided_at":     s.intSchema("Decided at (unix, optional)"),
	}, []string{})

	migItem := s.objectSchema(map[string]*Schema{
		"id":                 s.stringSchema("Migration ID"),
		"collection_id":      s.stringSchema("Collection ID"),
		"recommendation_id":  s.stringSchema("Recommendation ID"),
		"from_pool_id":       s.stringSchema("From pool ID"),
		"to_pool_id":         s.stringSchema("To pool ID"),
		"direction":          s.stringSchema("Direction"),
		"status":             s.stringSchema("Status"),
		"manifest":           s.stringSchema("Manifest"),
		"reason":             s.stringSchema("Reason"),
		"created_at":         s.intSchema("Created at (unix)"),
		"executed_at":        s.intSchema("Executed at (unix, optional)"),
		"retain_until":       s.intSchema("Retain until (unix, optional)"),
		"finalized_at":       s.intSchema("Finalized at (unix, optional)"),
		"undone_at":          s.intSchema("Undone at (unix, optional)"),
	}, []string{})

	// POST /api/curator/recommendations/generate
	s.addPath(spec, "/api/curator/recommendations/generate", "post", Operation{
		Summary:     "Generate recommendations",
		OperationID: "curatorGenerate",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"created": s.intSchema("Number created"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/curator/recommendations
	s.addPath(spec, "/api/curator/recommendations", "get", Operation{
		Summary:     "List recommendations",
		OperationID: "curatorList",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.queryParam("status", "Status filter", false),
			s.queryParam("direction", "Direction filter", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"recommendations": s.arraySchema(recItem),
					"total":           s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/curator/recommendations/{id}/approve
	s.addPath(spec, "/api/curator/recommendations/{id}/approve", "post", Operation{
		Summary:     "Approve recommendation",
		OperationID: "curatorApprove",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Recommendation ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"status": s.stringSchema("Status (approved)"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (not pending)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/curator/recommendations/{id}/reject
	s.addPath(spec, "/api/curator/recommendations/{id}/reject", "post", Operation{
		Summary:     "Reject recommendation",
		OperationID: "curatorReject",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Recommendation ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"status": s.stringSchema("Status (rejected)"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (not pending)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/curator/recommendations/{id}
	s.addPath(spec, "/api/curator/recommendations/{id}", "get", Operation{
		Summary:     "Get recommendation by ID",
		OperationID: "curatorGet",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Recommendation ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"recommendation": recItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/curator/migrations/execute/{recommendationId}
	s.addPath(spec, "/api/curator/migrations/execute/{recommendationId}", "post", Operation{
		Summary:     "Execute migration for recommendation",
		OperationID: "curatorMigrationExecute",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("recommendationId", "Recommendation ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"migration": migItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (not approved, no pool, etc.)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/curator/migrations
	s.addPath(spec, "/api/curator/migrations", "get", Operation{
		Summary:     "List migrations",
		OperationID: "curatorMigrationsList",
		Security:    s.bearerAuth(),
		Parameters: []Parameter{
			s.queryParam("status", "Status filter", false),
			s.queryParam("collection_id", "Collection ID filter", false),
		},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"migrations": s.arraySchema(migItem),
					"total":      s.intSchema("Total"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/curator/migrations/{id}/undo
	s.addPath(spec, "/api/curator/migrations/{id}/undo", "post", Operation{
		Summary:     "Undo migration",
		OperationID: "curatorMigrationUndo",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Migration ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"status": s.stringSchema("Status (undone)"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"409": {Description: "Conflict (not completed or window expired)"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/curator/migrations/{id}
	s.addPath(spec, "/api/curator/migrations/{id}", "get", Operation{
		Summary:     "Get migration by ID",
		OperationID: "curatorMigrationGet",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Migration ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"migration": migItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/curator/migrations/process-consents
	s.addPath(spec, "/api/curator/migrations/process-consents", "post", Operation{
		Summary:     "Process silent consents",
		OperationID: "curatorProcessConsents",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"created":  s.intSchema("Created"),
					"executed": s.intSchema("Executed"),
					"failed":   s.intSchema("Failed"),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Maintenance paths
func (s *Server) addMaintenancePaths(spec *OpenAPISpec) {
	// POST /api/maintenance/run
	s.addPath(spec, "/api/maintenance/run", "post", Operation{
		Summary:     "Run monthly maintenance",
		OperationID: "maintenanceRun",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: &Schema{Type: "object"}},
			}},
			"401": {Description: "Unauthorized"},
			"405": {Description: "Method Not Allowed"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/maintenance/status
	s.addPath(spec, "/api/maintenance/status", "get", Operation{
		Summary:     "Get maintenance status",
		OperationID: "maintenanceStatus",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"statuses": s.arraySchema(s.objectSchema(map[string]*Schema{
						"collection_id": s.stringSchema("Collection ID"),
						"last_scrub":    s.intSchema("Last scrub (unix)"),
						"next_scrub":    s.intSchema("Next scrub (unix)"),
						"last_smart":    s.intSchema("Last SMART (unix)"),
						"next_smart":    s.intSchema("Next SMART (unix)"),
					}, []string{})),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
}

// Supplychain paths
func (s *Server) addSupplychainPaths(spec *OpenAPISpec) {
	entryItem := s.objectSchema(map[string]*Schema{
		"id":                   s.stringSchema("Entry ID"),
		"name":                 s.stringSchema("Name"),
		"publisher":            s.stringSchema("Publisher"),
		"source_repo":          s.stringSchema("Source repo"),
		"image_ref":            s.stringSchema("Image ref"),
		"pinned_digest":        s.stringSchema("Pinned digest"),
		"signature_kind":       s.stringSchema("Signature kind"),
		"source_kind":          s.stringSchema("Source kind"),
		"unverified_confirmed": s.boolSchema("Unverified confirmed"),
		"created_at":           s.intSchema("Created at (unix)"),
		"updated_at":           s.intSchema("Updated at (unix)"),
	}, []string{})

	createEntryReq := s.objectSchema(map[string]*Schema{
		"name":                s.stringSchema("Name"),
		"publisher":           s.stringSchema("Publisher"),
		"source_repo":         s.stringSchema("Source repo"),
		"image_ref":           s.stringSchema("Image ref"),
		"pinned_digest":       s.stringSchema("Pinned digest"),
		"signature_kind":      s.stringSchema("Signature kind"),
		"source_kind":         s.stringSchema("Source kind"),
		"unverified_confirmed": s.boolSchema("Unverified confirmed"),
	}, []string{"name", "publisher", "source_repo", "image_ref", "pinned_digest", "signature_kind", "source_kind"})

	verifyReq := s.objectSchema(map[string]*Schema{
		"served_digest":    s.stringSchema("Served digest"),
		"signature_status": s.stringSchema("Signature status (verified/failed/not_provided)"),
	}, []string{"served_digest", "signature_status"})

	// POST /api/supplychain/entries
	s.addPath(spec, "/api/supplychain/entries", "post", Operation{
		Summary:     "Create supply chain entry",
		OperationID: "createSupplychainEntry",
		Security:    s.bearerAuth(),
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: createEntryReq},
		}},
		Responses: s.writeResponses(true),
	})
	// GET /api/supplychain/entries
	s.addPath(spec, "/api/supplychain/entries", "get", Operation{
		Summary:     "List supply chain entries",
		OperationID: "listSupplychainEntries",
		Security:    s.bearerAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"supplychain_entries": s.arraySchema(entryItem),
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// GET /api/supplychain/entries/{id}
	s.addPath(spec, "/api/supplychain/entries/{id}", "get", Operation{
		Summary:     "Get supply chain entry",
		OperationID: "getSupplychainEntry",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Entry ID")},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"supplychain_entry": entryItem,
				}, []string{})},
			}},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// DELETE /api/supplychain/entries/{id}
	s.addPath(spec, "/api/supplychain/entries/{id}", "delete", Operation{
		Summary:     "Delete supply chain entry",
		OperationID: "deleteSupplychainEntry",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Entry ID")},
		Responses: map[string]Response{
			"204": {Description: "No Content"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Not Found"},
			"500": {Description: "Internal Server Error"},
		},
	})
	// POST /api/supplychain/entries/{id}/verify
	s.addPath(spec, "/api/supplychain/entries/{id}/verify", "post", Operation{
		Summary:     "Verify supply chain entry",
		OperationID: "verifySupplychainEntry",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Entry ID")},
		RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
			"application/json": {Schema: verifyReq},
		}},
		Responses: map[string]Response{
			"200": {Description: "OK", Content: map[string]MediaType{
				"application/json": {Schema: s.objectSchema(map[string]*Schema{
					"allowed": s.boolSchema("Allowed"),
									"reason":  s.stringSchema("Reason"),
								}, []string{})},
							}},
							"400": {Description: "Bad Request"},
							"401": {Description: "Unauthorized"},
							"404": {Description: "Not Found"},
							"500": {Description: "Internal Server Error"},
						},
						})
					}

					// Backup paths
					func (s *Server) addBackupPaths(spec *OpenAPISpec) {
						repoItem := s.objectSchema(map[string]*Schema{
							"id":           s.stringSchema("Repo ID"),
							"name":         s.stringSchema("Repo name"),
							"target_type":  s.stringSchema("Target type"),
							"target_path":  s.stringSchema("Target path"),
							"config_json":  s.stringSchema("Config JSON"),
							"secret_name":  s.stringSchema("Secret name"),
							"created_at":   s.intSchema("Created at (unix)"),
							"updated_at":   s.intSchema("Updated at (unix)"),
						}, []string{})

						snapItem := s.objectSchema(map[string]*Schema{
							"id":            s.stringSchema("Snapshot ID"),
							"repo_id":       s.stringSchema("Repo ID"),
							"collection_id": s.stringSchema("Collection ID"),
							"snapshot_id":   s.stringSchema("Restic snapshot ID"),
							"created_at":    s.intSchema("Created at (unix)"),
						}, []string{})

						createRepoReq := s.objectSchema(map[string]*Schema{
							"name":        s.stringSchema("Repo name"),
							"target_type": s.stringSchema("Target type (local/s3/etc.)"),
							"target_path": s.stringSchema("Target path (required for local)"),
							"config":      s.stringSchema("Config (non-secret params)"),
							"secret_name": s.stringSchema("Vault secret name"),
						}, []string{"name"})

						runReq := s.objectSchema(map[string]*Schema{
							"collection_id": s.stringSchema("Collection ID"),
							"password":      s.stringSchema("Restic password"),
						}, []string{"collection_id", "password"})

						restoreReq := s.objectSchema(map[string]*Schema{
							"snapshot_id": s.stringSchema("Snapshot ID"),
							"rel_path":    s.stringSchema("Relative path"),
							"dest":        s.stringSchema("Destination path"),
							"password":    s.stringSchema("Restic password"),
							"confirm":     s.boolSchema("Confirm overwrite"),
						}, []string{"snapshot_id", "rel_path", "dest", "password", "confirm"})

						// POST /api/backups/repos
						s.addPath(spec, "/api/backups/repos", "post", Operation{
							Summary:     "Create backup repo",
							OperationID: "createBackupRepo",
							Security:    s.bearerAuth(),
							RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
								"application/json": {Schema: createRepoReq},
							}},
							Responses: s.writeResponses(true),
						})
						// GET /api/backups/repos
						s.addPath(spec, "/api/backups/repos", "get", Operation{
							Summary:     "List backup repos",
							OperationID: "listBackupRepos",
							Security:    s.bearerAuth(),
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"repos": s.arraySchema(repoItem),
										"total": s.intSchema("Total"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// POST /api/backups/repos/{id}/run
						s.addPath(spec, "/api/backups/repos/{id}/run", "post", Operation{
							Summary:     "Run backup",
							OperationID: "runBackup",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Repo ID")},
							RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
								"application/json": {Schema: runReq},
							}},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"snapshot":     snapItem,
										"snapshot_id":  s.stringSchema("Snapshot ID"),
										"source":       s.stringSchema("Source (canonical/dirty_shadows)"),
										"snapshot_ids": s.arraySchema(s.stringSchema("Snapshot ID")),
									}, []string{})},
								}},
								"400": {Description: "Bad Request"},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"409": {Description: "Conflict (cold tier)"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/backups/repos/{id}/snapshots
						s.addPath(spec, "/api/backups/repos/{id}/snapshots", "get", Operation{
							Summary:     "List backup snapshots",
							OperationID: "listBackupSnapshots",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Repo ID")},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"snapshots": s.arraySchema(snapItem),
										"total":     s.intSchema("Total"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// POST /api/backups/repos/{id}/restore
						s.addPath(spec, "/api/backups/repos/{id}/restore", "post", Operation{
							Summary:     "Restore from backup",
							OperationID: "restoreBackup",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Repo ID")},
							RequestBody: &RequestBody{Required: true, Content: map[string]MediaType{
								"application/json": {Schema: restoreReq},
							}},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"restored_path": s.stringSchema("Restored path"),
									}, []string{})},
								}},
								"400": {Description: "Bad Request"},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"409": {Description: "Conflict (confirm required)"},
								"500": {Description: "Internal Server Error"},
							},
						})
					}

					// Restore test paths
					func (s *Server) addRestoreTestPaths(spec *OpenAPISpec) {
						runItem := s.objectSchema(map[string]*Schema{
							"id":        s.stringSchema("Run ID"),
							"started_at": s.intSchema("Started at (unix)"),
							"samples":   s.intSchema("Samples"),
							"passed":    s.intSchema("Passed"),
							"failed":    s.intSchema("Failed"),
							"skipped":   s.intSchema("Skipped"),
							"alerts":    s.intSchema("Alerts"),
						}, []string{})

						alertItem := s.objectSchema(map[string]*Schema{
							"id":           s.stringSchema("Alert ID"),
							"run_id":       s.stringSchema("Run ID"),
							"repo_id":      s.stringSchema("Repo ID"),
							"snapshot_id":  s.stringSchema("Snapshot ID"),
							"rel_path":     s.stringSchema("Relative path"),
							"reason":       s.stringSchema("Reason"),
							"created_at":   s.intSchema("Created at (unix)"),
							"resolved":     s.intSchema("Resolved (0/1)"),
						}, []string{})

						// POST /api/restore-tests/run
						s.addPath(spec, "/api/restore-tests/run", "post", Operation{
							Summary:     "Run scheduled restore test",
							OperationID: "restoreTestRun",
							Security:    s.bearerAuth(),
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"run_id":  s.stringSchema("Run ID"),
										"samples": s.intSchema("Samples"),
										"passed":  s.intSchema("Passed"),
										"failed":  s.intSchema("Failed"),
										"skipped": s.intSchema("Skipped"),
										"alerts":  s.intSchema("Alerts"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"405": {Description: "Method Not Allowed"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/restore-tests/runs
						s.addPath(spec, "/api/restore-tests/runs", "get", Operation{
							Summary:     "List restore test runs",
							OperationID: "restoreTestRuns",
							Security:    s.bearerAuth(),
							Parameters: []Parameter{
								s.intQueryParam("limit", "Limit (default 50, max 500)", false),
							},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"runs":  s.arraySchema(runItem),
										"total": s.intSchema("Total"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/restore-tests/alerts
						s.addPath(spec, "/api/restore-tests/alerts", "get", Operation{
							Summary:     "List restore test alerts",
							OperationID: "restoreTestAlerts",
							Security:    s.bearerAuth(),
							Parameters: []Parameter{
								s.queryParam("resolved", "Resolved filter (true/false)", false),
							},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"alerts": s.arraySchema(alertItem),
										"total":  s.intSchema("Total"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/restore-tests/status
						s.addPath(spec, "/api/restore-tests/status", "get", Operation{
							Summary:     "Get restore test status",
							OperationID: "restoreTestStatus",
							Security:    s.bearerAuth(),
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"last_run_at":  s.intSchema("Last run at (unix, optional)"),
										"next_due_at":  s.intSchema("Next due at (unix, optional)"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
					}

					// Shadow promotion paths
					func (s *Server) addShadowPromotionPaths(spec *OpenAPISpec) {
						promoItem := s.objectSchema(map[string]*Schema{
							"id":                 s.stringSchema("Promotion ID"),
							"collection_id":      s.stringSchema("Collection ID"),
							"rel_path":           s.stringSchema("Relative path"),
							"shadow_pool_id":     s.stringSchema("Shadow pool ID"),
							"access_count_30d":   s.intSchema("Access count (30d)"),
							"status":             s.stringSchema("Status (pending/approved/rejected)"),
							"created_at":         s.intSchema("Created at (unix)"),
							"decided_at":         s.intSchema("Decided at (unix, optional)"),
						}, []string{})

						// POST /api/shadows/promotions/generate
						s.addPath(spec, "/api/shadows/promotions/generate", "post", Operation{
							Summary:     "Generate shadow promotions",
							OperationID: "shadowPromotionsGenerate",
							Security:    s.bearerAuth(),
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"created": s.intSchema("Number created"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/shadows/promotions
						s.addPath(spec, "/api/shadows/promotions", "get", Operation{
							Summary:     "List shadow promotions",
							OperationID: "shadowPromotionsList",
							Security:    s.bearerAuth(),
							Parameters: []Parameter{
								s.queryParam("status", "Status filter", false),
								s.intQueryParam("limit", "Limit (default 100)", false),
								s.intQueryParam("offset", "Offset (default 0)", false),
							},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"promotions": s.arraySchema(promoItem),
										"total":      s.intSchema("Total"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// POST /api/shadows/promotions/{id}/approve
						s.addPath(spec, "/api/shadows/promotions/{id}/approve", "post", Operation{
							Summary:     "Approve shadow promotion",
							OperationID: "shadowPromotionApprove",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Promotion ID")},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"status": s.stringSchema("Status (approve)"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"409": {Description: "Conflict (not pending)"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// POST /api/shadows/promotions/{id}/reject
						s.addPath(spec, "/api/shadows/promotions/{id}/reject", "post", Operation{
							Summary:     "Reject shadow promotion",
							OperationID: "shadowPromotionReject",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Promotion ID")},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"status": s.stringSchema("Status (reject)"),
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"409": {Description: "Conflict (not pending)"},
								"500": {Description: "Internal Server Error"},
							},
						})
						// GET /api/shadows/promotions/{id}
						s.addPath(spec, "/api/shadows/promotions/{id}", "get", Operation{
							Summary:     "Get shadow promotion by ID",
							OperationID: "shadowPromotionGet",
							Security:    s.bearerAuth(),
							Parameters:  []Parameter{s.pathParam("id", "Promotion ID")},
							Responses: map[string]Response{
								"200": {Description: "OK", Content: map[string]MediaType{
									"application/json": {Schema: s.objectSchema(map[string]*Schema{
										"promotion": promoItem,
									}, []string{})},
								}},
								"401": {Description: "Unauthorized"},
								"404": {Description: "Not Found"},
								"500": {Description: "Internal Server Error"},
							},
						})
					}

// Auto-Arrange paths (WS38 FR-2.11; served via the collections subtree dispatcher)
func (s *Server) addArrangePaths(spec *OpenAPISpec) {
	// POST /api/collections/{id}/arrange/plans (generate)
	s.addPath(spec, "/api/collections/{id}/arrange/plans", "post", Operation{
		Summary:     "Generate an auto-arrange plan",
		OperationID: "arrangePlanGenerate",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"201": {Description: "Created"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// GET /api/collections/{id}/arrange/plans (list)
	s.addPath(spec, "/api/collections/{id}/arrange/plans", "get", Operation{
		Summary:     "List auto-arrange plans",
		OperationID: "arrangePlanList",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// GET /api/collections/{id}/arrange/plans/{planId}
	s.addPath(spec, "/api/collections/{id}/arrange/plans/{planId}", "get", Operation{
		Summary:     "Get an auto-arrange plan",
		OperationID: "arrangePlanGet",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID"), s.pathParam("planId", "Plan ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Plan not found"},
		},
	})
	// POST .../approve, .../reject, .../apply, .../undo
	for _, action := range []struct {
		name, summary, opID string
	}{
		{"approve", "Approve an auto-arrange plan", "arrangePlanApprove"},
		{"reject", "Reject an auto-arrange plan", "arrangePlanReject"},
		{"apply", "Apply an auto-arrange plan", "arrangePlanApply"},
		{"undo", "Undo an applied auto-arrange plan", "arrangePlanUndo"},
	} {
		s.addPath(spec, "/api/collections/{id}/arrange/plans/{planId}/"+action.name, "post", Operation{
			Summary:     action.summary,
			OperationID: action.opID,
			Security:    s.bearerAuth(),
			Parameters:  []Parameter{s.pathParam("id", "Collection ID"), s.pathParam("planId", "Plan ID")},
			Responses: map[string]Response{
				"200": {Description: "OK"},
				"401": {Description: "Unauthorized"},
				"404": {Description: "Plan not found"},
				"409": {Description: "Conflict (wrong plan state)"},
			},
		})
	}
}

// AI tagging paths (WS39 FR-2.12; served via the collections subtree dispatcher)
func (s *Server) addTaggingPaths(spec *OpenAPISpec) {
	// POST /api/collections/{id}/tags/run
	s.addPath(spec, "/api/collections/{id}/tags/run", "post", Operation{
		Summary:     "Run AI tagging on a collection",
		OperationID: "tagsRun",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// GET /api/collections/{id}/tags/search
	s.addPath(spec, "/api/collections/{id}/tags/search", "get", Operation{
		Summary:     "Search files by tag",
		OperationID: "tagsSearch",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// POST /api/collections/{id}/tags
	s.addPath(spec, "/api/collections/{id}/tags", "post", Operation{
		Summary:     "Create a tag",
		OperationID: "tagsCreate",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"201": {Description: "Created"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// GET /api/collections/{id}/tags
	s.addPath(spec, "/api/collections/{id}/tags", "get", Operation{
		Summary:     "List tags",
		OperationID: "tagsList",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
	// DELETE /api/collections/{id}/tags
	s.addPath(spec, "/api/collections/{id}/tags", "delete", Operation{
		Summary:     "Delete tags",
		OperationID: "tagsDelete",
		Security:    s.bearerAuth(),
		Parameters:  []Parameter{s.pathParam("id", "Collection ID")},
		Responses: map[string]Response{
			"200": {Description: "OK"},
			"401": {Description: "Unauthorized"},
			"404": {Description: "Collection not found"},
		},
	})
}

// Web UI shell route (public, no auth)
func (s *Server) addUIPath(spec *OpenAPISpec) {
	s.addPath(spec, "/", "get", Operation{
		Summary:     "Serve the KelviOS web UI",
		OperationID: "uiShell",
		Security:    s.noAuth(),
		Responses: map[string]Response{
			"200": {Description: "OK"},
		},
	})
}
