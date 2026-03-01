package internal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type SearchFilters struct {
	Query      string `json:"query,omitempty"`
	Category   string `json:"category,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Verified   *bool  `json:"verified,omitempty"`
	PublicOnly bool   `json:"public_only"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

type SearchService struct {
	db *sql.DB
}

func NewSearchService(db *sql.DB) *SearchService {
	return &SearchService{db: db}
}

func (s *SearchService) SearchTemplates(ctx context.Context, filters SearchFilters) ([]Template, error) {
	if filters.Limit <= 0 || filters.Limit > 200 {
		filters.Limit = 50
	}
	if filters.Offset < 0 {
		filters.Offset = 0
	}
	if !filters.PublicOnly {
		filters.PublicOnly = true
	}

	conditions := []string{"1=1"}
	args := []interface{}{}
	argIndex := 1

	if filters.PublicOnly {
		conditions = append(conditions, "is_public=TRUE")
	}
	if strings.TrimSpace(filters.Query) != "" {
		conditions = append(conditions, fmt.Sprintf("(name ILIKE $%d OR description ILIKE $%d OR slug ILIKE $%d)", argIndex, argIndex, argIndex))
		args = append(args, "%"+strings.TrimSpace(filters.Query)+"%")
		argIndex++
	}
	if strings.TrimSpace(filters.Category) != "" {
		conditions = append(conditions, fmt.Sprintf("category=$%d", argIndex))
		args = append(args, strings.TrimSpace(filters.Category))
		argIndex++
	}
	if strings.TrimSpace(filters.Tag) != "" {
		conditions = append(conditions, fmt.Sprintf("$%d = ANY(tags)", argIndex))
		args = append(args, strings.TrimSpace(filters.Tag))
		argIndex++
	}
	if filters.Verified != nil {
		conditions = append(conditions, fmt.Sprintf("is_verified=$%d", argIndex))
		args = append(args, *filters.Verified)
		argIndex++
	}

	args = append(args, filters.Limit, filters.Offset)
	where := strings.Join(conditions, " AND ")
	query := fmt.Sprintf(`
		SELECT id::text, name, slug, description, category, tags::text,
		       agent_type, image, version, is_public, is_verified,
		       downloads, rating, created_at, updated_at
		FROM agent_templates
		WHERE %s
		ORDER BY downloads DESC, rating DESC, updated_at DESC
		LIMIT $%d OFFSET $%d`, where, argIndex, argIndex+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Template{}
	for rows.Next() {
		var item Template
		var tagsRaw string
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Slug,
			&item.Description,
			&item.Category,
			&tagsRaw,
			&item.AgentType,
			&item.Image,
			&item.Version,
			&item.IsPublic,
			&item.IsVerified,
			&item.Downloads,
			&item.Rating,
			&item.CreatedAt,
			&item.UpdatedAt,
		); err != nil {
			return nil, err
		}
		item.Tags = parsePGArray(tagsRaw)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
