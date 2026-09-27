import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { openLinkInSystemBrowser } from "../lib/external-link-policy";

export function JiraTicketCard({ item, projectName }: { item: components["schemas"]["BacklogItem"]; projectName?: string }) {
	const { t } = useTranslation();
	return <button type="button" data-testid="jira-ticket-card"
		className="flex w-full flex-col gap-2 rounded-md border border-dashed border-border-strong bg-surface/50 p-3 text-left text-xs hover:bg-surface focus-visible:outline focus-visible:outline-2 focus-visible:outline-settings-accent"
		onClick={() => void openLinkInSystemBrowser(item.url)}>
		<span className="flex w-full items-center justify-between gap-2 text-muted-foreground"><span>{item.key}</span><span className="rounded bg-blue-500/10 px-1.5 py-0.5 text-blue-500">{t("backlog.provider")}</span></span>
		<span className="font-medium text-foreground">{item.title}</span>
		<span className="text-muted-foreground">{item.status}</span>
		<span className="text-passive">{item.assignee || t("backlog.unassigned")}</span>
		{projectName && <span className="self-start rounded border border-border px-1.5 py-0.5 text-muted-foreground">{projectName}</span>}
	</button>;
}
