"""
Role-Based Access Control

Roles:  admin | developer | viewer
Each role has a fixed set of permissions. No per-user overrides (keep it simple and auditable).
"""
from enum import Enum
from typing import Callable
from fastapi import Depends, HTTPException

from security.auth import AuthenticatedUser, get_current_user


class Role(str, Enum):
    ADMIN     = "admin"
    DEVELOPER = "developer"
    VIEWER    = "viewer"


class Permission(str, Enum):
    CREATE_AGENT    = "create_agent"
    START_AGENT     = "start_agent"
    STOP_AGENT      = "stop_agent"
    DELETE_AGENT    = "delete_agent"
    VIEW_LOGS       = "view_logs"
    MANAGE_SCHEDULES = "manage_schedules"
    VIEW_METRICS    = "view_metrics"
    MANAGE_ALERTS   = "manage_alerts"
    ADMIN_ACCESS    = "admin_access"


ROLE_PERMISSIONS: dict[Role, set[Permission]] = {
    Role.ADMIN: {
        Permission.CREATE_AGENT,
        Permission.START_AGENT,
        Permission.STOP_AGENT,
        Permission.DELETE_AGENT,
        Permission.VIEW_LOGS,
        Permission.MANAGE_SCHEDULES,
        Permission.VIEW_METRICS,
        Permission.MANAGE_ALERTS,
        Permission.ADMIN_ACCESS,
    },
    Role.DEVELOPER: {
        Permission.CREATE_AGENT,
        Permission.START_AGENT,
        Permission.STOP_AGENT,
        Permission.VIEW_LOGS,
        Permission.MANAGE_SCHEDULES,
        Permission.VIEW_METRICS,
    },
    Role.VIEWER: {
        Permission.VIEW_LOGS,
        Permission.VIEW_METRICS,
    },
}


def has_permission(role: str, permission: Permission) -> bool:
    try:
        r = Role(role)
    except ValueError:
        return False
    return permission in ROLE_PERMISSIONS.get(r, set())


def require_permission(permission: Permission) -> Callable:
    async def _check(user: AuthenticatedUser = Depends(get_current_user)) -> AuthenticatedUser:
        if not has_permission(user.role, permission):
            raise HTTPException(
                status_code=403,
                detail=f"Role '{user.role}' does not have permission '{permission.value}'"
            )
        return user
    return _check
