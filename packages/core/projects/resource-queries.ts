import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { projectKeys } from "./queries";
import type {
  CreateProjectResourceRequest,
  ListProjectResourcesResponse,
  ProjectResource,
  UpdateProjectResourceRequest,
} from "../types";

export const projectResourceKeys = {
  list: (wsId: string, projectId: string) =>
    [...projectKeys.detail(wsId, projectId), "resources"] as const,
};

/** Workspace-scoped: the browse endpoint lives under /api/workspaces/{id}. */
export const localDirBrowseKeys = {
  browse: (wsId: string, path: string | null, includeHidden: boolean) =>
    ["workspaces", wsId, "local-dirs", { path, includeHidden }] as const,
};

/**
 * One level of the deployment machine's filesystem. The path is part of the
 * key so navigating up/down/home is just another query, and back-and-forth
 * navigation hits the cache. staleTime mirrors the other probe-the-machine
 * reads (runtimes/local-skills.ts): the listing changes out of band, so
 * re-opening a dialog after half a minute should refetch, but rapid
 * navigation inside a session should not hammer the server.
 */
export function localDirBrowseOptions(
  wsId: string,
  params: { path?: string; includeHidden?: boolean } = {},
) {
  const path = params.path ?? null;
  const includeHidden = params.includeHidden ?? false;
  return queryOptions({
    queryKey: localDirBrowseKeys.browse(wsId, path, includeHidden),
    queryFn: () =>
      api.browseLocalDirs(wsId, {
        path: path ?? undefined,
        includeHidden,
      }),
    staleTime: 30_000,
  });
}

export function projectResourcesOptions(wsId: string, projectId: string) {
  return queryOptions({
    queryKey: projectResourceKeys.list(wsId, projectId),
    queryFn: () => api.listProjectResources(projectId),
    select: (data) => data.resources,
  });
}

export function useCreateProjectResource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (data: CreateProjectResourceRequest) =>
      api.createProjectResource(projectId, data),
    onSuccess: (created) => {
      qc.setQueryData<ListProjectResourcesResponse>(
        projectResourceKeys.list(wsId, projectId),
        (old) =>
          old && !old.resources.some((r) => r.id === created.id)
            ? {
                ...old,
                resources: [...old.resources, created],
                total: old.total + 1,
              }
            : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({
        queryKey: projectResourceKeys.list(wsId, projectId),
      });
    },
  });
}

export function useUpdateProjectResource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({
      resourceId,
      data,
    }: {
      resourceId: string;
      data: UpdateProjectResourceRequest;
    }) => api.updateProjectResource(projectId, resourceId, data),
    onSuccess: (updated) => {
      qc.setQueryData<ListProjectResourcesResponse>(
        projectResourceKeys.list(wsId, projectId),
        (old) =>
          old
            ? {
                ...old,
                resources: old.resources.map((r) =>
                  r.id === updated.id ? updated : r,
                ),
              }
            : old,
      );
    },
    onSettled: () => {
      qc.invalidateQueries({
        queryKey: projectResourceKeys.list(wsId, projectId),
      });
    },
  });
}

export function useDeleteProjectResource(wsId: string, projectId: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (resourceId: string) =>
      api.deleteProjectResource(projectId, resourceId),
    onMutate: async (resourceId) => {
      await qc.cancelQueries({
        queryKey: projectResourceKeys.list(wsId, projectId),
      });
      const prev = qc.getQueryData<ListProjectResourcesResponse>(
        projectResourceKeys.list(wsId, projectId),
      );
      qc.setQueryData<ListProjectResourcesResponse>(
        projectResourceKeys.list(wsId, projectId),
        (old) =>
          old
            ? {
                ...old,
                resources: old.resources.filter(
                  (r: ProjectResource) => r.id !== resourceId,
                ),
                total: old.total - 1,
              }
            : old,
      );
      return { prev };
    },
    onError: (_err, _id, ctx) => {
      if (ctx?.prev) {
        qc.setQueryData(projectResourceKeys.list(wsId, projectId), ctx.prev);
      }
    },
    onSettled: () => {
      qc.invalidateQueries({
        queryKey: projectResourceKeys.list(wsId, projectId),
      });
    },
  });
}
