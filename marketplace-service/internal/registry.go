package internal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type RegistryVersion struct {
	ID          string                 `json:"id"`
	TemplateID  string                 `json:"template_id"`
	Version     string                 `json:"version"`
	Image       string                 `json:"image"`
	ImageDigest string                 `json:"image_digest,omitempty"`
	Config      map[string]interface{} `json:"config"`
	IsLatest    bool                   `json:"is_latest"`
	PublishedAt time.Time              `json:"published_at"`
	CreatedAt   time.Time              `json:"created_at"`
}

type RegistryService struct {
	db *sql.DB
}

func NewRegistryService(db *sql.DB) *RegistryService {
	return &RegistryService{db: db}
}

func (s *RegistryService) PublishVersion(ctx context.Context, templateID, version, image, imageDigest string, config map[string]interface{}) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = tx.Rollback()
	}()
	id, err := s.PublishVersionTx(ctx, tx, templateID, version, image, imageDigest, config)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *RegistryService) PublishVersionTx(ctx context.Context, tx *sql.Tx, templateID, version, image, imageDigest string, config map[string]interface{}) (string, error) {
	if templateID == "" || version == "" || image == "" {
		return "", errors.New("template_id, version, and image are required")
	}
	cfgJSON, _ := json.Marshal(defaultMap(config))

	_, err := tx.ExecContext(ctx, `
		UPDATE agent_registry
		SET is_latest=FALSE
		WHERE template_id=$1::uuid`, templateID)
	if err != nil {
		return "", err
	}

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO agent_registry
			(template_id, version, image, image_digest, config, is_latest, published_at, created_at)
		VALUES
			($1::uuid, $2, $3, $4, $5::jsonb, TRUE, NOW(), NOW())
		ON CONFLICT (template_id, version) DO UPDATE SET
			image=EXCLUDED.image,
			image_digest=EXCLUDED.image_digest,
			config=EXCLUDED.config,
			is_latest=TRUE,
			published_at=NOW()
		RETURNING id::text`,
		templateID, version, image, imageDigest, string(cfgJSON)).Scan(&id)
	if err != nil {
		return "", err
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE agent_templates
		SET version=$1, image=$2, updated_at=NOW()
		WHERE id=$3::uuid`, version, image, templateID)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (s *RegistryService) ListVersions(ctx context.Context, slug string) ([]RegistryVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ar.id::text, ar.template_id::text, ar.version, ar.image, ar.image_digest,
		       ar.config, ar.is_latest, ar.published_at, ar.created_at
		FROM agent_registry ar
		JOIN agent_templates at ON at.id=ar.template_id
		WHERE at.slug=$1
		ORDER BY ar.published_at DESC`, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RegistryVersion{}
	for rows.Next() {
		var item RegistryVersion
		var configRaw []byte
		if err := rows.Scan(
			&item.ID, &item.TemplateID, &item.Version, &item.Image, &item.ImageDigest,
			&configRaw, &item.IsLatest, &item.PublishedAt, &item.CreatedAt,
		); err != nil {
			return nil, err
		}
		item.Config = map[string]interface{}{}
		_ = json.Unmarshal(configRaw, &item.Config)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
