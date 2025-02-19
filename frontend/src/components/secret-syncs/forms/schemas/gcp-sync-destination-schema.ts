import { z } from "zod";

import { BaseSecretSyncSchema } from "@app/components/secret-syncs/forms/schemas/base-secret-sync-schema";
import { SecretSync } from "@app/hooks/api/secretSyncs";
import { GcpSyncScope } from "@app/hooks/api/secretSyncs/types/gcp-sync";

export const GcpSyncDestinationSchema = BaseSecretSyncSchema().merge(
  z.object({
    destination: z.literal(SecretSync.GCPSecretManager),
    destinationConfig: z.object({
      scope: z.literal(GcpSyncScope.Global),
      projectId: z.string().min(1, "Project ID required")
    })
  })
);
