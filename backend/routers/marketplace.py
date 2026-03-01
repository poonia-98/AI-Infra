import json
from typing import Any, Optional
from fastapi import APIRouter, Depends, HTTPException, Query
from pydantic import BaseModel, Field
from sqlalchemy.ext.asyncio import AsyncSession
from sqlalchemy import text
from database import get_db


router = APIRouter(prefix="/api/v1/marketplace", tags=["marketplace"])


class TemplateCreate(BaseModel):
    organisation_id: Optional[str] = None
    name: str
    slug: str
    description: Optional[str] = None
    category: Optional[str] = None
    tags: list[str] = Field(default_factory=list)
    agent_type: str = "langgraph"
    image: str
    config_schema: dict[str, Any] = Field(default_factory=dict)
    default_config: dict[str, Any] = Field(default_factory=dict)
    default_env: dict[str, Any] = Field(default_factory=dict)
    version: str = "1.0.0"
    is_public: bool = True
    created_by: Optional[str] = None


class InstallRequest(BaseModel):
    config: dict[str, Any] = Field(default_factory=dict)
    env_vars: dict[str, Any] = Field(default_factory=dict)


@router.get("/templates")
async def list_templates(
    q: Optional[str] = None,
    category: Optional[str] = None,
    tag: Optional[str] = None,
    verified: Optional[bool] = None,
    limit: int = Query(50, ge=1, le=200),
    db: AsyncSession = Depends(get_db),
):
    clauses = ["is_public=TRUE"]
    params: dict[str, Any] = {"limit": limit}
    idx = 1
    if q:
        clauses.append(f"(name ILIKE :q OR description ILIKE :q OR slug ILIKE :q)")
        params["q"] = f"%{q}%"
        idx += 1
    if category:
        clauses.append("category=:category")
        params["category"] = category
        idx += 1
    if tag:
        clauses.append(":tag = ANY(tags)")
        params["tag"] = tag
        idx += 1
    if verified is not None:
        clauses.append("is_verified=:verified")
        params["verified"] = verified

    rows = await db.execute(
        text(
            f"""
            SELECT id::text, organisation_id::text, name, slug, description, category, tags,
                   agent_type, image, config_schema, default_config, default_env,
                   version, is_public, is_verified, downloads, rating, rating_count,
                   created_by, created_at, updated_at
            FROM agent_templates
            WHERE {" AND ".join(clauses)}
            ORDER BY downloads DESC, rating DESC, updated_at DESC
            LIMIT :limit
            """
        ),
        params,
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.get("/templates/{slug}")
async def get_template(slug: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(
        text(
            """
            SELECT id::text, organisation_id::text, name, slug, description, category, tags,
                   agent_type, image, config_schema, default_config, default_env,
                   version, is_public, is_verified, downloads, rating, rating_count,
                   readme, created_by, created_at, updated_at
            FROM agent_templates
            WHERE slug=:slug
            """
        ),
        {"slug": slug},
    )
    row = rows.fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Template not found")
    return dict(row._mapping)


@router.post("/templates", status_code=201)
async def create_template(payload: TemplateCreate, db: AsyncSession = Depends(get_db)):
    result = await db.execute(
        text(
            """
            INSERT INTO agent_templates
              (organisation_id, name, slug, description, category, tags, agent_type, image,
               config_schema, default_config, default_env, version, is_public, created_by)
            VALUES
              (:organisation_id::uuid, :name, :slug, :description, :category, :tags::text[],
               :agent_type, :image, :config_schema::jsonb, :default_config::jsonb, :default_env::jsonb,
               :version, :is_public, :created_by)
            RETURNING id::text
            """
        ),
        {
            "organisation_id": payload.organisation_id,
            "name": payload.name,
            "slug": payload.slug,
            "description": payload.description,
            "category": payload.category,
            "tags": "{" + ",".join(payload.tags) + "}" if payload.tags else "{}",
            "agent_type": payload.agent_type,
            "image": payload.image,
            "config_schema": json.dumps(payload.config_schema),
            "default_config": json.dumps(payload.default_config),
            "default_env": json.dumps(payload.default_env),
            "version": payload.version,
            "is_public": payload.is_public,
            "created_by": payload.created_by,
        },
    )
    row = result.fetchone()
    template_id = row[0] if row else ""

    await db.execute(
        text(
            """
            INSERT INTO agent_registry (template_id, version, image, is_latest, config)
            VALUES (:template_id::uuid, :version, :image, TRUE, :config::jsonb)
            ON CONFLICT (template_id, version) DO UPDATE
            SET image=EXCLUDED.image, is_latest=TRUE, config=EXCLUDED.config, published_at=NOW()
            """
        ),
        {"template_id": template_id, "version": payload.version, "image": payload.image, "config": json.dumps({})},
    )
    return {"id": template_id}


@router.post("/templates/{slug}/install")
async def install_template(slug: str, payload: InstallRequest, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(
        text(
            """
            SELECT id::text, image, agent_type, version, default_config, default_env
            FROM agent_templates
            WHERE slug=:slug
            """
        ),
        {"slug": slug},
    )
    row = rows.fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Template not found")

    default_config = row[4] or {}
    default_env = row[5] or {}
    merged_config = dict(default_config)
    merged_config.update(payload.config)
    merged_env = dict(default_env)
    merged_env.update(payload.env_vars)

    await db.execute(
        text("UPDATE agent_templates SET downloads=downloads+1, updated_at=NOW() WHERE id=:id::uuid"),
        {"id": row[0]},
    )
    return {
        "template_id": row[0],
        "slug": slug,
        "image": row[1],
        "agent_type": row[2],
        "version": row[3],
        "config": merged_config,
        "env_vars": merged_env,
    }


@router.get("/templates/{slug}/versions")
async def list_template_versions(slug: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(
        text(
            """
            SELECT ar.id::text, ar.version, ar.image, ar.image_digest, ar.config,
                   ar.is_latest, ar.published_at, ar.created_at
            FROM agent_registry ar
            JOIN agent_templates at ON at.id=ar.template_id
            WHERE at.slug=:slug
            ORDER BY ar.published_at DESC
            """
        ),
        {"slug": slug},
    )
    return [dict(r._mapping) for r in rows.fetchall()]


@router.post("/templates/{slug}/publish")
async def publish_version(slug: str, version: str, image: str, db: AsyncSession = Depends(get_db)):
    rows = await db.execute(text("SELECT id::text FROM agent_templates WHERE slug=:slug"), {"slug": slug})
    row = rows.fetchone()
    if not row:
        raise HTTPException(status_code=404, detail="Template not found")
    template_id = row[0]
    await db.execute(text("UPDATE agent_registry SET is_latest=FALSE WHERE template_id=:template_id::uuid"), {"template_id": template_id})
    result = await db.execute(
        text(
            """
            INSERT INTO agent_registry (template_id, version, image, is_latest, config)
            VALUES (:template_id::uuid, :version, :image, TRUE, '{}'::jsonb)
            ON CONFLICT (template_id, version) DO UPDATE
            SET image=EXCLUDED.image, is_latest=TRUE, published_at=NOW()
            RETURNING id::text
            """
        ),
        {"template_id": template_id, "version": version, "image": image},
    )
    rid = result.fetchone()[0]
    await db.execute(
        text("UPDATE agent_templates SET version=:version, image=:image, updated_at=NOW() WHERE id=:template_id::uuid"),
        {"template_id": template_id, "version": version, "image": image},
    )
    return {"id": rid, "template_id": template_id, "version": version}
