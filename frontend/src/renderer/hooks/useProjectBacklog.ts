import { queryOptions, useQuery } from "@tanstack/react-query";
import { apiClient, apiErrorMessage } from "../lib/api-client";

export function projectBacklogQueryOptions(projectId?: string) {
	return queryOptions({
		queryKey: projectId ? ["project-backlog", projectId] : ["project-backlog"],
		staleTime: 60_000,
		refetchInterval: 60_000,
		retry: false,
		// Provider backoff belongs to the daemon. Polls during Retry-After only read its cache.
		queryFn: async () => {
			const { data, error } = projectId
				? await apiClient.GET("/api/v1/projects/{id}/backlog", { params: { path: { id: projectId } } })
				: await apiClient.GET("/api/v1/projects/backlog");
			if (error) throw new Error(apiErrorMessage(error));
			if (!data) throw new Error("Missing backlog response");
			return data;
		},
	});
}

export function useProjectBacklog(projectId?: string, enabled = true) {
	return useQuery({ ...projectBacklogQueryOptions(projectId), enabled });
}
