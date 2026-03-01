package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Template struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Slug          string                 `json:"slug"`
	Description   string                 `json:"description"`
	Category      string                 `json:"category"`
	Tags          []string               `json:"tags"`
	AgentType     string                 `json:"agent_type"`
	Image         string                 `json:"image"`
	ConfigSchema  map[string]interface{} `json:"config_schema"`
	DefaultConfig map[string]interface{} `json:"default_config"`
	DefaultEnv    map[string]interface{} `json:"default_env"`
	Version       string                 `json:"version"`
	IsPublic      bool                   `json:"is_public"`
	IsVerified    bool                   `json:"is_verified"`
	Downloads     int                    `json:"downloads"`
	Rating        float64                `json:"rating"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

type TemplateCreateInput struct {
	Name          string                 `json:"name"`
	Slug          string                 `json:"slug"`
	Description   string                 `json:"description"`
	Category      string                 `json:"category"`
	Tags          []string               `json:"tags"`
	AgentType     string                 `json:"agent_type"`
	Image         string                 `json:"image"`
	ConfigSchema  map[string]interface{} `json:"config_schema"`
	DefaultConfig map[string]interface{} `json:"default_config"`
	DefaultEnv    map[string]interface{} `json:"default_env"`
	Version       string                 `json:"version"`
	IsPublic      bool                   `json:"is_public"`
	CreatedBy     string                 `json:"created_by"`
}

type InstallResult struct {
	TemplateID  string                 `json:"template_id"`
	Slug        string                 `json:"slug"`
	Image       string                 `json:"image"`
	AgentType   string                 `json:"agent_type"`
	Config      map[string]interface{} `json:"config"`
	EnvVars     map[string]interface{} `json:"env_vars"`
	Version     string                 `json:"version"`
	InstalledAt time.Time              `json:"installed_at"`
}

type TemplateService struct {
	db       *sql.DB
	registry *RegistryService
}

func NewTemplateService(db *sql.DB) *TemplateService {
	registry := NewRegistryService(db)
	return &TemplateService{
		db:       db,
		registry: registry,
	}
}

func (s *TemplateService) Publish(ctx context.Context, in TemplateCreateInput) (string, error) {
	if in.Name == "" || in.Slug == "" || in.Image == "" {
		return "", errors.New("name, slug, and image are required")
	}
	if in.AgentType == "" {
		in.AgentType = "langgraph"
	}
	if in.Version == "" {
		in.Version = "1.0.0"
	}

	schemaJSON, _ := json.Marshal(defaultMap(in.ConfigSchema))
	configJSON, _ := json.Marshal(defaultMap(in.DefaultConfig))
	envJSON, _ := json.Marshal(defaultMap(in.DefaultEnv))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_templates
			(name, slug, description, category, tags, agent_type, image,
			 config_schema, default_config, default_env, version, is_public, created_by)
		VALUES
			($1, $2, $3, $4, $5::text[], $6, $7, $8::jsonb, $9::jsonb, $10::jsonb, $11, $12, $13)
		ON CONFLICT (slug) DO UPDATE SET
			name=EXCLUDED.name,
			description=EXCLUDED.description,
			category=EXCLUDED.category,
			tags=EXCLUDED.tags,
			agent_type=EXCLUDED.agent_type,
			image=EXCLUDED.image,
			config_schema=EXCLUDED.config_schema,
			default_config=EXCLUDED.default_config,
			default_env=EXCLUDED.default_env,
			version=EXCLUDED.version,
			is_public=EXCLUDED.is_public,
			updated_at=NOW()
		RETURNING id::text`,
		in.Name, in.Slug, in.Description, in.Category, pqStringArray(in.Tags), in.AgentType, in.Image,
		string(schemaJSON), string(configJSON), string(envJSON), in.Version, in.IsPublic, in.CreatedBy).Scan(&id)
	if err != nil {
		return "", err
	}

	if _, err := s.registry.PublishVersionTx(ctx, tx, id, in.Version, in.Image, "", map[string]interface{}{}); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *TemplateService) GetBySlug(ctx context.Context, slug string) (*Template, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id::text, name, slug, description, category, tags::text, agent_type,
		       image, config_schema, default_config, default_env, version,
		       is_public, is_verified, downloads, rating, created_at, updated_at
		FROM agent_templates
		WHERE slug=$1`, slug)

	var item Template
	var tagsRaw string
	var schemaRaw, configRaw, envRaw []byte
	err := row.Scan(
		&item.ID, &item.Name, &item.Slug, &item.Description, &item.Category, &tagsRaw,
		&item.AgentType, &item.Image, &schemaRaw, &configRaw, &envRaw, &item.Version,
		&item.IsPublic, &item.IsVerified, &item.Downloads, &item.Rating, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	item.Tags = parsePGArray(tagsRaw)
	item.ConfigSchema = map[string]interface{}{}
	item.DefaultConfig = map[string]interface{}{}
	item.DefaultEnv = map[string]interface{}{}
	_ = json.Unmarshal(schemaRaw, &item.ConfigSchema)
	_ = json.Unmarshal(configRaw, &item.DefaultConfig)
	_ = json.Unmarshal(envRaw, &item.DefaultEnv)
	return &item, nil
}

func (s *TemplateService) Install(ctx context.Context, slug string, overrides map[string]interface{}, envOverrides map[string]interface{}) (*InstallResult, error) {
	template, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if template == nil {
		return nil, errors.New("template not found")
	}

	config := mergeMap(template.DefaultConfig, overrides)
	env := mergeMap(template.DefaultEnv, envOverrides)

	_, _ = s.db.ExecContext(ctx, `UPDATE agent_templates SET downloads=downloads+1, updated_at=NOW() WHERE id=$1::uuid`, template.ID)

	return &InstallResult{
		TemplateID:  template.ID,
		Slug:        template.Slug,
		Image:       template.Image,
		AgentType:   template.AgentType,
		Config:      config,
		EnvVars:     env,
		Version:     template.Version,
		InstalledAt: time.Now().UTC(),
	}, nil
}

func mergeMap(base map[string]interface{}, overrides map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func defaultMap(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return map[string]interface{}{}
	}
	return input
}

func pqStringArray(values []string) string {
	if len(values) == 0 {
		return "{}"
	}
	out := "{"
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf("\"%s\"", v)
	}
	out += "}"
	return out
}

func parsePGArray(raw string) []string {
	if raw == "" || raw == "{}" {
		return []string{}
	}
	raw = strings.TrimPrefix(raw, "{")
	raw = strings.TrimSuffix(raw, "}")
	if raw == "" {
		return []string{}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(p, "\" ")
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
