import { and, desc, eq, lt } from "@workspace/db";
import { adminAuditLog, user } from "@workspace/db/schema";
import { db } from "@workspace/db/server";

export interface GetAdminActionsInput {
  limit: number;
  /** Return entries older than this id: the last id of the previous page. */
  before?: number;
  actorId?: string;
}

/** Newest first, with the actor's current name and email. */
export const getAdminActions = async ({
  limit,
  before,
  actorId,
}: GetAdminActionsInput) =>
  db
    .select({
      id: adminAuditLog.id,
      actorId: adminAuditLog.actorId,
      actorRole: adminAuditLog.actorRole,
      actorName: user.name,
      actorEmail: user.email,
      action: adminAuditLog.action,
      targetType: adminAuditLog.targetType,
      targetId: adminAuditLog.targetId,
      details: adminAuditLog.details,
      ipAddress: adminAuditLog.ipAddress,
      createdAt: adminAuditLog.createdAt,
    })
    .from(adminAuditLog)
    .leftJoin(user, eq(user.id, adminAuditLog.actorId))
    .where(
      and(
        before === undefined ? undefined : lt(adminAuditLog.id, before),
        actorId === undefined ? undefined : eq(adminAuditLog.actorId, actorId),
      ),
    )
    .orderBy(desc(adminAuditLog.id))
    .limit(limit);
