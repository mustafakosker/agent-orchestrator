import { fireEvent, render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
const open = vi.hoisted(() => vi.fn());
vi.mock("../lib/external-link-policy", () => ({ openLinkInSystemBrowser: open }));
import { JiraTicketCard } from "./JiraTicketCard";
it("shows native workflow status, unassigned and a project badge, and opens the system browser", () => {
	render(<JiraTicketCard item={{ key: "ACME-1", title: "Ticket title", status: "Selected for Development", url: "https://jira.example/browse/ACME-1", projectId: "p1" }} projectName="Project One" />);
	expect(screen.getByText("Selected for Development")).toBeInTheDocument();
	expect(screen.getByText("Unassigned")).toBeInTheDocument();
	expect(screen.getByText("Project One")).toBeInTheDocument();
	expect(screen.getByText("Jira")).toBeInTheDocument();
	fireEvent.click(screen.getByRole("button"));
	expect(open).toHaveBeenCalledWith("https://jira.example/browse/ACME-1");
});
