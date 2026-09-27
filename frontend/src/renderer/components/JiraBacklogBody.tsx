import { useTranslation } from "react-i18next";
import type { components } from "../../api/schema";
import { useUiStore } from "../stores/ui-store";
import { AppLink } from "./AppLink";

type Backlog = components["schemas"]["BacklogResponse"];
type ProjectChoice = { id: string; name: string };
const actionClass = "text-settings-accent hover:underline text-left";

function BacklogProblem({ reason, message, projectId, projects }: {
	reason?: string; message?: string; projectId?: string; projects: ProjectChoice[];
}) {
	const { t } = useTranslation();
	const openSettings = useUiStore((state) => state.openProjectSettings);
	if (reason === "config_absent") {
		if (projectId) return <button className={actionClass} onClick={() => openSettings(projectId, "backlog")}>{t("backlog.connect")}</button>;
		return <div className="flex flex-col gap-2">
			<p>{t(projects.length ? "backlog.chooseProject" : "backlog.needsProject")}</p>
			{projects.map((project) => <button key={project.id} className={actionClass} onClick={() => openSettings(project.id, "backlog")}>{t("backlog.connectProject", { project: project.name })}</button>)}
		</div>;
	}
	if (reason === "credentials_missing") return <div className="flex flex-col gap-2">
		<p>{t("settings.project.backlogCredentialsMissing")}</p>
		<AppLink className={actionClass} href="https://github.com/aoagents/agent-orchestrator/blob/main/docs/superpowers/specs/2026-09-26-jira-board-column-design.md#authentication" target="_blank" rel="noopener noreferrer">{t("settings.project.backlogCredentialsDocs")}</AppLink>
	</div>;
	if (reason === "auth_failed") return <p>{t("backlog.reauthenticate")}</p>;
	if (reason === "config_invalid") return <div className="flex flex-col gap-2">
		<p>{t("backlog.configInvalid")}</p><p>{message}</p>
		{projectId && <button className={actionClass} onClick={() => openSettings(projectId, "backlog")}>{t("backlog.settings")}</button>}
	</div>;
	return <p>{t("backlog.unavailable")}</p>;
}

export function JiraBacklogBody({ data, isError = false, projectId, projects }: {
	data?: Backlog; isError?: boolean; projectId?: string; projects: ProjectChoice[];
}) {
	const { t, i18n } = useTranslation();
	const failures = data?.failures ?? [];
	return <div className="flex flex-col gap-3 text-xs text-muted-foreground" role="status">
		{!data ? <p>{t(isError ? "backlog.unavailable" : "backlog.loading")}</p> : <>
			{data.state === "ok" ? <>
				{(data.stale || isError) && data.fetchedAt && <p>{t("backlog.asOf", { time: new Date(data.fetchedAt).toLocaleString(i18n.language) })}</p>}
				{isError && <p>{t("backlog.unavailable")}</p>}
				{data.items.length === 0 && <p>{t("backlog.empty")}</p>}
			</> : failures.length === 0 && <BacklogProblem reason={data.reason} message={data.message} projectId={projectId} projects={projects} />}
			{failures.map((failure) => <div key={failure.projectId} className="flex flex-col gap-1 rounded border border-border p-2">
				<p className="font-medium">{projects.find((project) => project.id === failure.projectId)?.name ?? failure.projectId}</p>
				<BacklogProblem {...failure} projects={projects} />
			</div>)}
		</>}
	</div>;
}
