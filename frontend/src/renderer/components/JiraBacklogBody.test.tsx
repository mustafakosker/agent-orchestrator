import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { components } from "../../api/schema";
import { useUiStore } from "../stores/ui-store";
import { JiraBacklogBody } from "./JiraBacklogBody";
type Backlog = components["schemas"]["BacklogResponse"];
const base: Backlog = { state: "ok", items: [], stale: false, fetchedAt: null };
const projects = [{ id: "p1", name: "Project One" }];
function show(data?: Backlog, extra = {}) { return render(<JiraBacklogBody data={data} projectId="p1" projects={projects} {...extra} />); }
describe("backlog lane states", () => {
	it("distinguishes loading and transport failure from an empty success", () => {
		const view = show();
		expect(screen.getByText("Loading Jira tickets…")).toBeInTheDocument();
		view.rerender(<JiraBacklogBody isError projects={projects} />);
		expect(screen.getByText("Jira is unavailable. Try refreshing later.")).toBeInTheDocument();
		expect(screen.queryByText("No ready tickets")).not.toBeInTheDocument();
	});
	it("shows an honest empty success", () => { show(base); expect(screen.getByText("No ready tickets")).toBeInTheDocument(); expect(screen.queryByRole("button")).not.toBeInTheDocument(); });
	it("offers a settings action for absent config", () => {
		show({ ...base, state: "not_configured", reason: "config_absent" });
		fireEvent.click(screen.getByRole("button", { name: "Connect Jira" }));
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "p1", section: "backlog" });
	});
	it("names missing credentials without offering a settings action", () => {
		show({ ...base, state: "not_configured", reason: "credentials_missing" });
		expect(screen.getByText(/AO_JIRA_API_TOKEN/)).toBeInTheDocument();
		expect(screen.getByRole("link")).toHaveAttribute("href", expect.stringContaining("#authentication"));
		expect(screen.queryByRole("button")).not.toBeInTheDocument();
	});
	it.each([
		["auth_failed", "Reauthenticate Jira", false],
		["config_invalid", "Jira configuration is invalid", true],
		["unavailable", "Jira is unavailable. Try refreshing later.", false],
	] as const)("shows %s distinctly", (reason, message, settings) => {
		show({ ...base, state: "error", reason, message: "Provider detail" });
		expect(screen.getByText(message)).toBeInTheDocument();
		expect(screen.queryByText("No ready tickets")).not.toBeInTheDocument();
		if (settings) { expect(screen.getByText("Provider detail")).toBeInTheDocument(); fireEvent.click(screen.getByRole("button", { name: "Backlog settings" })); expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "p1", section: "backlog" }); }
		else expect(screen.queryByRole("button")).not.toBeInTheDocument();
	});
	it("shows retained data age on server stale or client refresh failure", () => {
		show({ ...base, fetchedAt: "2026-09-26T10:00:00Z", stale: true });
		expect(screen.getByText(/As of/)).toBeInTheDocument();
	});
	it("reports individual aggregate failures while successful tickets remain usable", () => {
		show({ ...base, items: [{ key: "ACME-1", title: "Healthy", status: "Ready", projectId: "p1", url: "https://jira.example/ACME-1" }], failures: [{ projectId: "p2", state: "error", reason: "config_invalid", message: "Unknown project" }] }, { projectId: undefined, projects: [...projects, { id: "p2", name: "Project Two" }] });
		expect(screen.getByText("Project Two")).toBeInTheDocument();
		expect(screen.getByText("Unknown project")).toBeInTheDocument();
		fireEvent.click(screen.getByRole("button", { name: "Backlog settings" }));
		expect(useUiStore.getState().settingsModal).toEqual({ scope: "project", projectId: "p2", section: "backlog" });
		expect(screen.queryByText("No ready tickets")).not.toBeInTheDocument();
	});
	it("explains an aggregate with no eligible project instead of opening arbitrary settings", () => {
		show({ ...base, state: "not_configured", reason: "config_absent" }, { projectId: undefined, projects: [] });
		expect(screen.getByText("Add a repository project to connect Jira.")).toBeInTheDocument();
		expect(screen.queryByRole("button")).not.toBeInTheDocument();
	});
});
