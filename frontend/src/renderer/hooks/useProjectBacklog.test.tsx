import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
const get = vi.hoisted(() => vi.fn());
vi.mock("../lib/api-client", () => ({ apiClient: { GET: get }, apiErrorMessage: () => "offline" }));
import { projectBacklogQueryOptions, useProjectBacklog } from "./useProjectBacklog";

describe("project backlog query", () => {
	beforeEach(() => get.mockReset().mockResolvedValue({ data: { state: "ok", items: [], stale: false, fetchedAt: null } }));
	it("uses distinct scoped and aggregate endpoints with a 60-second local polling cadence", async () => {
		const scoped = projectBacklogQueryOptions("p1");
		const all = projectBacklogQueryOptions();
		expect(scoped.queryKey).toEqual(["project-backlog", "p1"]);
		expect(all.queryKey).toEqual(["project-backlog"]);
		expect(scoped.refetchInterval).toBe(60_000);
		expect(scoped.staleTime).toBe(60_000);
		expect(scoped.retry).toBe(false);
		const client = new QueryClient();
		await client.fetchQuery(scoped);
		await client.fetchQuery(all);
		expect(get.mock.calls).toEqual([
			["/api/v1/projects/{id}/backlog", { params: { path: { id: "p1" } } }],
			["/api/v1/projects/backlog"],
		]);
		client.clear();
	});
	it("retains tickets after a failed refresh and isolates data when projects change", async () => {
		const data = { state: "ok", items: [{ key: "ACME-1" }], stale: false, fetchedAt: "2026-09-26T10:00:00Z" };
		get.mockResolvedValueOnce({ data });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
		const { result, rerender, unmount } = renderHook(({ id }) => useProjectBacklog(id), { wrapper, initialProps: { id: "p1" } });
		await waitFor(() => expect(result.current.data).toEqual(data));
		// Subscribe to the error flag before the refresh (TanStack tracks accessed fields).
		expect(result.current.isError).toBe(false);
		get.mockRejectedValueOnce(new Error("offline"));
		await act(async () => { await result.current.refetch(); });
		expect(result.current.data).toEqual(data);
		await waitFor(() => expect(result.current.isError).toBe(true));
		rerender({ id: "p2" });
		expect(result.current.data).toBeUndefined();
		await waitFor(() => expect(result.current.data?.items).toEqual([]));
		unmount(); client.clear();
	});
	it("does not fetch for unsupported or hidden boards", () => {
		const client = new QueryClient();
		const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
		const { unmount } = renderHook(() => useProjectBacklog("scratch", false), { wrapper });
		expect(get).not.toHaveBeenCalled();
		unmount(); client.clear();
	});
});
