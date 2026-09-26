import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import { cn } from "../lib/utils";
import { AppLink } from "./AppLink";
import { SettingsInlineInput, SettingsRow } from "./settings/SettingsRow";
import { Switch } from "./ui/switch";

type TrackerBacklogConfig = NonNullable<components["schemas"]["ProjectConfig"]["trackerBacklog"]>;

export type BacklogForm = {
	enabled: boolean;
	site: string;
	projectKey: string;
	statuses: string;
};

function splitStatuses(value: string): string[] {
	return value.split(",").map((status) => status.trim()).filter(Boolean);
}

export function backlogNeedsScope(form: BacklogForm): boolean {
	return form.enabled && (!form.site.trim() || !form.projectKey.trim() || splitStatuses(form.statuses).length === 0);
}

export function buildBacklog(form: BacklogForm, existing?: TrackerBacklogConfig): TrackerBacklogConfig | undefined {
	const statuses = splitStatuses(form.statuses);
	const site = form.site.trim() || undefined;
	const projectKey = form.projectKey.trim() || undefined;
	const next: TrackerBacklogConfig = {
		...existing,
		enabled: form.enabled || undefined,
		provider: form.enabled || site || projectKey || statuses.length ? "jira" : undefined,
		site,
		projectKey,
		statuses: statuses.length ? statuses : undefined,
	};
	return Object.values(next).some((value) => value !== undefined) ? next : undefined;
}

export function JiraBacklogFields({
	form, onChange, compact = false, variant = "default", controlClassName, labelClassName,
}: {
	form: BacklogForm;
	onChange: (patch: Partial<BacklogForm>) => void;
	compact?: boolean;
	variant?: "default" | "settings";
	controlClassName?: string;
	labelClassName?: string;
}) {
	const { t } = useTranslation();
	const fields = [
		{ key: "site", label: t("settings.project.backlogSite"), placeholder: t("settings.project.backlogSitePlaceholder") },
		{ key: "projectKey", label: t("settings.project.backlogProjectKey"), placeholder: t("settings.project.backlogProjectKeyPlaceholder") },
		{ key: "statuses", label: t("settings.project.backlogStatuses"), placeholder: t("settings.project.backlogStatusesPlaceholder") },
	] as const;
	const scopeError = !form.site.trim() ? t("settings.project.backlogSiteRequired")
		: !form.projectKey.trim() ? t("settings.project.backlogProjectKeyRequired")
		: t("settings.project.backlogStatusesRequired");
	return (
		<div className="flex flex-col gap-1.5">
			{!compact && <p className="px-1 text-xs text-muted-foreground">{t("settings.project.backlogDescription")}</p>}
			<SettingsRow label={t("settings.project.enableBacklog")}>
				<Switch aria-label={t("settings.project.enableBacklog")} checked={form.enabled} onCheckedChange={(enabled) => onChange({ enabled })} />
			</SettingsRow>
			{form.enabled && <>
				{fields.map(({ key, label, placeholder }) => variant === "settings" ? (
					<SettingsRow key={key} label={label}>
						<SettingsInlineInput id={`backlog-${key}`} label={label} value={form[key]} onChange={(value) => onChange({ [key]: value })} placeholder={placeholder} />
					</SettingsRow>
				) : (
					<label key={key} className={cn("flex flex-col gap-1.5 text-xs text-muted-foreground", labelClassName)}>
						{label}
						<input className={cn("h-control-form rounded-md border border-input bg-transparent px-2.5 text-control text-foreground", controlClassName)} value={form[key]} placeholder={placeholder} onChange={(event) => onChange({ [key]: event.target.value })} />
					</label>
				))}
				{!compact && <p className="px-1 text-xs text-muted-foreground">{t("settings.project.backlogStatusesHelp")}</p>}
				{!compact && backlogNeedsScope(form) && <p className="flex items-center gap-1.5 px-1 text-xs text-error">
					<TriangleAlert className="size-3 shrink-0" aria-hidden="true" />{scopeError}
				</p>}
			</>}
		</div>
	);
}

// Only the saved, enabled configuration is probed. Editing an unsaved form
// must never send its values to Jira or turn missing credentials into a save error.
export function JiraBacklogCredentialsNotice({ projectId }: { projectId: string }) {
	const { t } = useTranslation();
	const query = useQuery({
		queryKey: ["project-backlog", projectId],
		staleTime: 60_000,
		retry: false,
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects/{id}/backlog", { params: { path: { id: projectId } } });
			if (error) throw new Error(apiErrorMessage(error));
			return data;
		},
	});
	if (query.data?.reason !== "credentials_missing") return null;
	return <div className="px-1 text-xs text-muted-foreground" role="status">
		<p>{t("settings.project.backlogCredentialsMissing")}</p>
		<AppLink className="text-settings-accent hover:underline" href="https://github.com/aoagents/agent-orchestrator/blob/main/docs/superpowers/specs/2026-09-26-jira-board-column-design.md#authentication" target="_blank" rel="noopener noreferrer">
			{t("settings.project.backlogCredentialsDocs")}
		</AppLink>
	</div>;
}
