import { useEffect, useMemo, useRef, useState } from "react";
import {
  Archive,
  Bot,
  CheckCircle2,
  ChevronDown,
  CircleSlash,
  Clock3,
  ExternalLink,
  FileText,
  HeartPulse,
  Inbox,
  ListTodo,
  Loader2,
  MessageSquare,
  MoreVertical,
  Pause,
  Pencil,
  Pin,
  RefreshCw,
  Send,
  Settings,
  ShieldCheck,
  Sparkles,
  X,
} from "lucide-react";
import "./styles.css";

const API_BASE = import.meta.env.VITE_API_BASE ?? "http://127.0.0.1:8787";

function api(path, options = {}) {
  return fetch(`${API_BASE}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...options,
  }).then(async (response) => {
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) {
      throw new Error(payload.error || `Request failed: ${response.status}`);
    }
    return payload;
  });
}

export function App() {
  const [snapshot, setSnapshot] = useState(null);
  const [selected, setSelected] = useState(null);
  const [tab, setTab] = useState("overview");
  const [activeSection, setActiveSection] = useState("review");
  const [draftText, setDraftText] = useState("");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const refreshInFlight = useRef(null);

  const refresh = async () => {
    if (refreshInFlight.current) {
      return refreshInFlight.current;
    }
    refreshInFlight.current = api("/api/dashboard")
      .then((data) => {
        setSnapshot(data);
        setSelected((current) => (resolveSelected(data, current) ? current : pickInitialSelection(data)));
        return data;
      })
      .finally(() => {
        refreshInFlight.current = null;
      });
    return refreshInFlight.current;
  };

  const refreshAfterCurrent = async () => {
    const current = refreshInFlight.current;
    if (current) {
      await current.catch(() => {});
    }
    return refresh();
  };

  useEffect(() => {
    refresh().catch((err) => setError(err.message));
    const timer = setInterval(() => refresh().catch(() => {}), 8000);
    return () => clearInterval(timer);
  }, []);

  const selectedDetail = useMemo(() => resolveSelected(snapshot, selected), [snapshot, selected]);

  useEffect(() => {
    if (selectedDetail?.reply) {
      setDraftText(selectedDetail.reply.edited_text || selectedDetail.reply.draft_text || "");
    }
  }, [selectedDetail?.reply?.id]);

  const runAction = async (label, fn) => {
    setBusy(label);
    setError("");
    try {
      await fn();
      await refreshAfterCurrent();
    } catch (err) {
      setError(err.message);
    } finally {
      setBusy("");
    }
  };

  const counts = snapshot?.counts ?? {};
  const health = snapshot?.health ?? [];
  const replyDrafts = useMemo(() => sortBySlackActivity(snapshot?.reply_drafts), [snapshot?.reply_drafts]);
  const selectSection = (section) => {
    setActiveSection(section);
    const nextSelection = pickSectionSelection(snapshot, section);
    if (nextSelection) setSelected(nextSelection);
  };

  return (
    <div className="app-shell">
      <aside className="sidebar">
        <div className="brand">
          <div className="brand-icon">
            <Bot size={22} />
          </div>
          <div>
            <div className="brand-title">Local Slack Agent</div>
            <div className="brand-status">
              <span className="status-dot good" />
              Running
            </div>
          </div>
        </div>

        <nav className="nav">
          <NavItem icon={<Inbox size={18} />} label="Review" count={counts.drafts ?? 0} active={activeSection === "review"} onClick={() => selectSection("review")} />
          <NavItem icon={<ListTodo size={18} />} label="Queue" count={counts.queued ?? 0} active={activeSection === "queue"} onClick={() => selectSection("queue")} />
          <NavItem icon={<Clock3 size={18} />} label="Working" count={counts.active ?? 0} active={activeSection === "working"} onClick={() => selectSection("working")} />
          <NavItem icon={<CircleSlash size={18} />} label="Blocked" count={counts.blocked ?? 0} active={activeSection === "blocked"} onClick={() => selectSection("blocked")} />
          <NavItem icon={<FileText size={18} />} label="Intake" count={counts.intake ?? counts.threads ?? 0} active={activeSection === "intake"} onClick={() => selectSection("intake")} />
          <NavItem icon={<Archive size={18} />} label="Archive" count={counts.archive ?? 0} active={activeSection === "archive"} onClick={() => selectSection("archive")} />
          <NavItem icon={<HeartPulse size={18} />} label="Health" active={activeSection === "health"} onClick={() => selectSection("health")} />
        </nav>

        <div className="sidebar-spacer" />

        <div className="sidebar-health">
          <HealthMini label="Slack" checks={health} service="slack" />
          <HealthMini label="Codex" checks={health} service="codex_app_server" />
          <HealthMini label="Bootstrap" checks={health} service="bootstrap_command" />
          <div className="local-note">
            <ShieldCheck size={15} />
            All data is local
          </div>
        </div>

        <button className="settings-button" type="button" onClick={() => selectSection("health")}>
          <Settings size={17} />
          Settings
        </button>
      </aside>

      <main className="workbench">
        <header className="topbar">
          <div>
            <h1>Slack agent workbench</h1>
          </div>
          <div className="topbar-actions">
            {error && <span className="error-pill">{error}</span>}
            <button
              className="ghost-button"
              type="button"
              onClick={() => runAction("health", () => api("/api/health"))}
              disabled={busy === "health"}
            >
              {busy === "health" ? <Loader2 size={16} className="spin" /> : <HeartPulse size={16} />}
              Health
            </button>
            <button
              className="primary-lite-button"
              type="button"
              onClick={() => runAction("sync", () => api("/api/sync", { method: "POST" }))}
              disabled={busy === "sync"}
            >
              {busy === "sync" ? <Loader2 size={16} className="spin" /> : <RefreshCw size={16} />}
              Sync Slack
            </button>
          </div>
        </header>

        {activeSection === "review" && (
        <section className="queue-section active-view" id="section-review">
          <SectionTitle title="Reply drafts ready for review" count={replyDrafts.length} />
          <div className="table-shell">
            <div className="draft-table header">
              <span>Priority</span>
              <span>Job</span>
              <span>From</span>
              <span>Analyzer</span>
              <span>Confidence</span>
              <span>Activity</span>
              <span>Slack</span>
            </div>
            {replyDrafts.map((draft) => (
              <SelectableRow
                key={draft.id}
                className="draft-table row"
                selected={selected?.type === "reply" && selected?.id === draft.id}
                onSelect={() => setSelected({ type: "reply", id: draft.id })}
              >
                <Priority value="high" />
                <span>
                  <strong>{draft.job_title}</strong>
                  <small>Reply draft</small>
                </span>
                <span>
                  <strong>{actorName(draft)}</strong>
                  <small>{sourceMeta(draft)}</small>
                </span>
                <span>
                  <StateBadge tone="good">{analyzerBadgeLabel(draft)}</StateBadge>
                </span>
                <span className="confidence">
                  {hasConfidence(draft.confidence) && <Progress value={Number(draft.confidence)} />}
                  {formatConfidence(draft.confidence)}
                </span>
                <span title={dateTimeFull(draft.latest_slack_message_ts)}>
                  {displayActivityTime(draft.latest_slack_message_ts)}
                </span>
                <SlackLinkButton item={draft} />
              </SelectableRow>
            ))}
            {snapshot && replyDrafts.length === 0 && <EmptyRow text="No reply drafts waiting." />}
          </div>
        </section>
        )}

        {activeSection === "intake" && (
        <section className="queue-section active-view" id="section-intake">
          <SectionTitle title="Slack intake" count={counts.intake ?? snapshot?.intake?.length ?? 0} />
          <div className="table-shell">
            <div className="intake-table header">
              <span>From</span>
              <span>Trigger</span>
              <span>Channel</span>
              <span>Status</span>
              <span>Activity</span>
              <span>Slack</span>
            </div>
            {(snapshot?.intake ?? []).map((thread) => (
              <SelectableRow
                key={thread.id}
                className="intake-table row"
                selected={selected?.type === "thread" && selected?.id === thread.id}
                onSelect={() => setSelected({ type: "thread", id: thread.id })}
              >
                <span>
                  <strong>{actorName(thread)}</strong>
                  <small>{sourceLabel(thread.source_type)}</small>
                </span>
                <span>
                  <strong>{thread.title || "Untitled intake item"}</strong>
                  <small>{thread.thread_ts}</small>
                </span>
                <span>
                  <strong>{thread.channel_name || thread.channel_id}</strong>
                  <small>{thread.channel_id}</small>
                </span>
                <span>
                  <StateBadge tone="neutral">
                    {statusLabel(thread.status)}
                  </StateBadge>
                </span>
                <span title={dateTimeFull(thread.latest_slack_message_ts)}>{activityTime(thread.latest_slack_message_ts)}</span>
                <SlackLinkButton item={thread} />
              </SelectableRow>
            ))}
            {snapshot && snapshot.intake?.length === 0 && <EmptyRow text="No Slack intake waiting." />}
          </div>
        </section>
        )}

        {activeSection === "blocked" && (
        <section className="queue-section active-view" id="section-blocked">
          <SectionTitle title="Blocked" count={snapshot?.blocked?.length ?? 0} />
          <div className="table-shell compact">
            <div className="blocked-table header">
              <span>Job</span>
              <span>From</span>
              <span>Reason</span>
              <span>Age</span>
              <span>Slack</span>
            </div>
            {(snapshot?.blocked ?? []).map((job) => (
              <SelectableRow
                key={`${job.item_type || "job"}-${job.id}`}
                className="blocked-table row"
                selected={isSelectedBlocked(selected, job)}
                onSelect={() => setSelected(blockedSelection(job))}
              >
                <span>
                  <strong>{job.title}</strong>
                  <small>{job.item_type === "thread" ? "Analyzer failed" : "Waiting on input"}</small>
                </span>
                <span>
                  <strong>{actorName(job)}</strong>
                  <small>{sourceMeta(job)}</small>
                </span>
                <span>{job.current_block_reason || "Needs user confirmation"}</span>
                <span className="age">{relativeTime(job.updated_at)}</span>
                <SlackLinkButton item={job} />
              </SelectableRow>
            ))}
            {snapshot && snapshot.blocked?.length === 0 && <EmptyRow text="No blocked jobs." />}
          </div>
        </section>
        )}

        {activeSection === "queue" && (
        <section className="queue-section active-view" id="section-queue">
          <SectionTitle title="Queued jobs" count={snapshot?.queued?.length ?? 0} />
          <div className="table-shell compact">
            <div className="worker-table header">
              <span>Queue</span>
              <span>Job</span>
              <span>Workspace</span>
              <span>Status</span>
              <span>Age</span>
              <span>Slack</span>
            </div>
            {(snapshot?.queued ?? []).map((job) => (
              <SelectableRow
                key={job.id}
                className="worker-table row"
                selected={selected?.type === "job" && selected?.id === job.id}
                onSelect={() => setSelected({ type: "job", id: job.id })}
              >
                <span className="worker-name">
                  <span className="status-dot" />
                  job-{job.id}
                </span>
                <span>{job.title}</span>
                <span className="mono">{job.workspace_path || "pending"}</span>
                <span>{statusLabel(job.status)}</span>
                <span className="age">{relativeTime(job.updated_at)}</span>
                <SlackLinkButton item={job} />
              </SelectableRow>
            ))}
            {snapshot && snapshot.queued?.length === 0 && <EmptyRow text="No jobs queued." />}
          </div>
        </section>
        )}

        {activeSection === "working" && (
        <section className="queue-section active-view" id="section-working">
          <SectionTitle title="Active workers" count={snapshot?.active?.length ?? 0} />
          <div className="table-shell compact">
            <div className="worker-table header">
              <span>Worker</span>
              <span>Job</span>
              <span>Workspace</span>
              <span>Step</span>
              <span>Progress</span>
              <span>Slack</span>
            </div>
            {(snapshot?.active ?? []).map((job) => (
              <SelectableRow
                key={job.id}
                className="worker-table row"
                selected={selected?.type === "job" && selected?.id === job.id}
                onSelect={() => setSelected({ type: "job", id: job.id })}
              >
                <span className="worker-name">
                  <span className="status-dot good" />
                  codex-{job.id}
                </span>
                <span>{job.title}</span>
                <span className="mono">{job.workspace_path || "pending"}</span>
                <span>{statusLabel(job.status)}</span>
                <Progress value={job.status === "working" ? 0.62 : 0.24} />
                <SlackLinkButton item={job} />
              </SelectableRow>
            ))}
            {snapshot && snapshot.active?.length === 0 && <EmptyRow text="No workers running right now." />}
          </div>
        </section>
        )}

        {activeSection === "archive" && (
        <section className="queue-section active-view" id="section-archive">
          <SectionTitle title="Archive" count={counts.archive ?? snapshot?.archive?.length ?? 0} />
          <div className="table-shell compact">
            <div className="intake-table header">
              <span>From</span>
              <span>Thread</span>
              <span>Channel</span>
              <span>Status</span>
              <span>Activity</span>
              <span>Slack</span>
            </div>
            {(snapshot?.archive ?? []).map((thread) => (
              <SelectableRow
                key={thread.id}
                className="intake-table row"
                selected={selected?.type === "thread" && selected?.id === thread.id}
                onSelect={() => setSelected({ type: "thread", id: thread.id })}
              >
                <span>
                  <strong>{actorName(thread)}</strong>
                  <small>{sourceLabel(thread.source_type)}</small>
                </span>
                <span>
                  <strong>{thread.title || "Untitled intake item"}</strong>
                  <small>{thread.thread_ts}</small>
                </span>
                <span>
                  <strong>{thread.channel_name || thread.channel_id}</strong>
                  <small>{thread.channel_id}</small>
                </span>
                <span>
                  <StateBadge tone="good">{statusLabel(thread.status)}</StateBadge>
                </span>
                <span title={dateTimeFull(thread.latest_slack_message_ts)}>{activityTime(thread.latest_slack_message_ts)}</span>
                <SlackLinkButton item={thread} />
              </SelectableRow>
            ))}
            {snapshot && snapshot.archive?.length === 0 && <EmptyRow text="No archived items yet." />}
          </div>
        </section>
        )}

        {activeSection === "health" && (
        <section className="queue-section active-view" id="section-health">
          <SectionTitle title="Health and settings" count={health.length} />
          <div className="table-shell compact">
            <div className="health-table header">
              <span>Service</span>
              <span>Status</span>
              <span>Detail</span>
              <span>Checked</span>
            </div>
            {health.map((check) => (
              <div className="health-table row static-row" key={check.service}>
                <span>
                  <strong>{check.service}</strong>
                  <small>local check</small>
                </span>
                <span>
                  <StateBadge tone={healthTone(check.status)}>{statusLabel(check.status)}</StateBadge>
                </span>
                <span className="mono">{check.detail || "No detail"}</span>
                <span>{timeOnly(check.checked_at)}</span>
              </div>
            ))}
            {health.length === 0 && <EmptyRow text="Run Health to populate local checks." />}
          </div>
        </section>
        )}
      </main>

      <aside className="detail-pane">
        <div className="detail-top">
          <h2>{selectedDetail?.thread && !selectedDetail?.job && !selectedDetail?.reply ? "Thread details" : "Job details"}</h2>
          <div className="icon-actions">
            <button type="button" title="Pin">
              <Pin size={16} />
            </button>
            <button type="button" title="More">
              <MoreVertical size={16} />
            </button>
            <button type="button" title="Close">
              <X size={16} />
            </button>
          </div>
        </div>

        {selectedDetail ? (
          <>
            <div className="detail-headline">
              <StateBadge tone="info">{selectedDetail.thread?.channel_name || selectedDetail.reply?.channel_name || "Slack"}</StateBadge>
              <h3>{selectedDetail.job?.title || selectedDetail.reply?.job_title || selectedDetail.thread?.title}</h3>
              <p className={selectedDetail.job?.status === "blocked" ? "status-text blocked" : "status-text"}>
                {detailStatus(selectedDetail)}
              </p>
            </div>

            <div className="meta-row">
              <Avatar name={detailActor(selectedDetail)} />
              <span>{detailActor(selectedDetail)}</span>
              <span>{detailSource(selectedDetail)}</span>
              <span>{detailTime(selectedDetail)}</span>
            </div>

            <div className="detail-scroll">
              {selectedDetail.reply && (
                <>
                  <ReplyReviewSummary detail={selectedDetail} draftText={draftText} />
                  <ReplyPanel
                    detail={selectedDetail}
                    draftText={draftText}
                    setDraftText={setDraftText}
                    runAction={runAction}
                    busy={busy}
                  />
                </>
              )}

              <DetailTabs tab={tab} setTab={setTab} compact={Boolean(selectedDetail.reply)} />

              <DetailContent
                detail={selectedDetail}
                tab={tab}
                onAnalyze={(threadID) => runAction("analyze", () => api(`/api/intake/${threadID}/analyze`, { method: "POST" }))}
                onRunJob={(jobID) => runAction("run-job", () => api(`/api/jobs/${jobID}/run`, { method: "POST" }))}
                busy={busy}
              />
            </div>
          </>
        ) : (
          <div className="empty-detail">
            <Sparkles size={22} />
            <p>Select a draft, blocked job, or active worker.</p>
          </div>
        )}
      </aside>
    </div>
  );
}

function DetailContent({ detail, tab, onAnalyze, onRunJob, busy }) {
  const slackLink = slackPermalink(detail.thread);

  if (tab === "activity") {
    return (
      <section className="detail-card">
        <h4>Activity</h4>
        {(detail.events ?? []).map((event) => (
          <div className="event-line" key={event.id}>
            <CheckCircle2 size={15} />
            <span>{event.message}</span>
            <small>{timeOnly(event.created_at)}</small>
          </div>
        ))}
        {detail.events?.length === 0 && <p className="muted">No worker events yet.</p>}
      </section>
    );
  }

  if (tab === "workspace") {
    return (
      <section className="detail-card">
        <h4>Workspace</h4>
        <InfoRow label="Path" value={detail.job?.workspace_path || "Not created yet"} mono />
        <InfoRow label="Bootstrap" value={detail.job?.bootstrap_status || "Pending"} />
        <InfoRow label="Codex thread" value={detail.job?.codex_thread_id || "Not started"} mono />
      </section>
    );
  }

  if (tab === "worker") {
    return (
      <section className="detail-card">
        <h4>Worker status</h4>
        <InfoRow label="State" value={statusLabel(detail.job?.status)} />
        <InfoRow label="Blocker" value={detail.job?.current_block_reason || "None"} />
        <InfoRow label="Next action" value={detail.job?.next_user_action || "No user action needed"} />
        {detail.job && (
          <button
            className="primary-lite-button full"
            type="button"
            onClick={() => onRunJob(detail.job.id)}
            disabled={busy === "run-job" || !canRunJob(detail.job)}
          >
            {busy === "run-job" ? <Loader2 size={16} className="spin" /> : <RefreshCw size={16} />}
            {workerActionLabel(detail.job)}
          </button>
        )}
        {canAnalyzeThread(detail.thread, detail.job) && (
          <button
            className="primary-lite-button full"
            type="button"
            onClick={() => onAnalyze(detail.thread.id)}
            disabled={busy === "analyze"}
          >
            {busy === "analyze" ? <Loader2 size={16} className="spin" /> : <RefreshCw size={16} />}
            Analyze intake
          </button>
        )}
      </section>
    );
  }

  if (tab === "analyzer") {
    return <AnalyzerPanel detail={detail} mode="full" />;
  }

  return (
    <>
      <AnalyzerPanel detail={detail} mode="summary" />
      <section className="detail-card">
        <h4>Slack intake</h4>
        <p className="thread-text">{slackThreadText(detail)}</p>
        {slackLink && (
          <a className="external-link" href={slackLink} target="_blank" rel="noreferrer">
            View in Slack <ExternalLink size={14} />
          </a>
        )}
        {canAnalyzeThread(detail.thread, detail.job) && (
          <button
            className="primary-lite-button full"
            type="button"
            onClick={() => onAnalyze(detail.thread.id)}
            disabled={busy === "analyze"}
          >
            {busy === "analyze" ? <Loader2 size={16} className="spin" /> : <RefreshCw size={16} />}
            Analyze intake
          </button>
        )}
      </section>
    </>
  );
}

function DetailTabs({ tab, setTab, compact }) {
  return (
    <div className={`tabs ${compact ? "compact-tabs" : ""}`}>
      {["overview", "analyzer", "worker", "workspace", "activity"].map((item) => (
        <button
          key={item}
          className={tab === item ? "active" : ""}
          type="button"
          onClick={() => setTab(item)}
        >
          {compact && item === "overview" ? "Evidence" : capitalize(item)}
        </button>
      ))}
    </div>
  );
}

function ReplyReviewSummary({ detail, draftText }) {
  const analysis = buildAnalysisView(detail);
  const fields = analysis.fields ?? [];
  const why = analysisValue(fields, "why_it_matters") || analysis.summary;
  const worker = analysisValue(fields, "worker_plan") || latestEvent(detail) || workerStatusSummary(detail);
  const risk = replyDraftLooksUnsafe(draftText)
    ? "Draft includes worker notes. Edit before sending."
    : analysisValue(fields, "needed_user_confirmation") || "Review destination and wording before sending.";
  const originalAsk = slackThreadText(detail);

  return (
    <section className="review-summary">
      <div className="review-summary-head">
        <div>
          <h4>Review decision</h4>
          <p>Everything needed for the Slack send decision is here.</p>
        </div>
        <StateBadge tone={replyDraftLooksUnsafe(draftText) ? "warn" : "good"}>
          {replyDraftLooksUnsafe(draftText) ? "Needs edit" : "Ready to review"}
        </StateBadge>
      </div>
      <div className="review-facts">
        <ReviewFact label="Why this reply exists" value={why} />
        <ReviewFact label="Worker checked" value={worker} />
        <ReviewFact label="Risk or confirmation" value={risk} tone={replyDraftLooksUnsafe(draftText) ? "warn" : ""} />
        <ReviewFact label="Original Slack ask" value={originalAsk} mono={looksLikeIdentifier(originalAsk)} />
      </div>
      {hasConfidence(analysis.confidence) && (
        <div className="review-confidence">
          <span>Analyzer confidence {formatConfidence(analysis.confidence)}</span>
          <Progress value={Number(analysis.confidence)} />
        </div>
      )}
    </section>
  );
}

function ReviewFact({ label, value, tone, mono }) {
  return (
    <div className={`review-fact ${tone || ""}`}>
      <span>{label}</span>
      <p className={mono ? "mono" : ""}>{value || "Not available yet."}</p>
    </div>
  );
}

function ReplyPanel({ detail, draftText, setDraftText, runAction, busy }) {
  const reply = detail.reply;
  return (
    <div className="reply-panel primary-review-panel">
      <div className="panel-title">
        <h4>Reply draft</h4>
        <button
          className="mini-button"
          type="button"
          onClick={() =>
            runAction("save-draft", () =>
              api(`/api/replies/${reply.id}`, {
                method: "PATCH",
                body: JSON.stringify({ text: draftText }),
              }),
            )
          }
        >
          <Pencil size={14} />
          Save
        </button>
      </div>
      <textarea
        value={draftText}
        onChange={(event) => setDraftText(event.target.value)}
        aria-label="Reply draft"
      />
      {replyDraftLooksUnsafe(draftText) && (
        <div className="draft-warning">Not Slack-ready: this draft includes worker notes.</div>
      )}
      <div className="reply-actions">
        <button
          className="secondary-button archive-button"
          type="button"
          onClick={() => runAction("archive-reply", () => api(`/api/replies/${reply.id}/archive`, { method: "POST" }))}
          disabled={busy === "archive-reply"}
        >
          {busy === "archive-reply" ? <Loader2 size={16} className="spin" /> : <Archive size={16} />}
          Ignore & archive
        </button>
        <button className="secondary-button" type="button">
          <MessageSquare size={16} />
          Request changes
        </button>
        <button
          className="send-button"
          type="button"
          onClick={() =>
            runAction("send", async () => {
              await api(`/api/replies/${reply.id}`, {
                method: "PATCH",
                body: JSON.stringify({ text: draftText }),
              });
              await api(`/api/replies/${reply.id}/send`, { method: "POST" });
            })
          }
          disabled={busy === "send" || !draftText.trim() || replyDraftLooksUnsafe(draftText)}
        >
          {busy === "send" ? <Loader2 size={17} className="spin" /> : <Send size={17} />}
          Approve & send
          <ChevronDown size={16} />
        </button>
      </div>
    </div>
  );
}

function NavItem({ icon, label, count, active, onClick }) {
  return (
    <button className={`nav-item ${active ? "active" : ""}`} type="button" onClick={onClick}>
      {icon}
      <span>{label}</span>
      {count !== undefined && <b>{count}</b>}
    </button>
  );
}

function SectionTitle({ title, count }) {
  return (
    <div className="section-title">
      <h2>{title}</h2>
      <span>{count}</span>
    </div>
  );
}

function EmptyRow({ text }) {
  return <div className="empty-row">{text}</div>;
}

function SelectableRow({ className, selected, onSelect, children }) {
  const handleKeyDown = (event) => {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    onSelect();
  };

  return (
    <div
      className={`${className} ${selected ? "selected" : ""}`}
      role="button"
      tabIndex={0}
      onClick={onSelect}
      onKeyDown={handleKeyDown}
    >
      {children}
    </div>
  );
}

function SlackLinkButton({ item }) {
  const permalink = slackPermalink(item);
  const label = slackLinkLabel(item);
  if (!permalink) {
    return (
      <span className="slack-link-button disabled" title="Slack link unavailable" aria-label="Slack link unavailable">
        <ExternalLink size={15} />
      </span>
    );
  }

  return (
    <a
      className="slack-link-button"
      href={permalink}
      target="_blank"
      rel="noreferrer"
      title="Open in Slack"
      aria-label={`Open ${label} in Slack`}
      onClick={(event) => event.stopPropagation()}
      onKeyDown={(event) => event.stopPropagation()}
    >
      <ExternalLink size={15} />
    </a>
  );
}

function Priority({ value }) {
  return (
    <span className={`priority ${value}`}>
      <span className="priority-dot" />
      {capitalize(value)}
    </span>
  );
}

function StateBadge({ children, tone = "neutral" }) {
  return <span className={`badge ${tone}`}>{children}</span>;
}

function Progress({ value }) {
  const width = `${Math.max(4, Math.min(100, Number(value || 0) * 100))}%`;
  return (
    <span className="progress">
      <span style={{ width }} />
    </span>
  );
}

function Avatar({ name }) {
  return <span className="avatar">{name?.slice(0, 1) || "A"}</span>;
}

function InfoRow({ label, value, mono }) {
  return (
    <div className="info-row">
      <span>{label}</span>
      <strong className={mono ? "mono" : ""}>{value}</strong>
    </div>
  );
}

function AnalyzerPanel({ detail, mode }) {
  const analysis = buildAnalysisView(detail);
  const full = mode === "full";
  return (
    <section className="detail-card analyzer-card">
      <h4>{full ? "Analyzer rationale" : "Analyzer summary"}</h4>
      <p className="analysis-summary">{analysis.summary}</p>
      {full && analysis.fields.length > 0 && (
        <div className="analysis-fields">
          {analysis.fields.map((field) => (
            <div className="analysis-field" key={field.key}>
              <span>{field.label}</span>
              <p>{field.value}</p>
            </div>
          ))}
        </div>
      )}
      {hasConfidence(analysis.confidence) ? (
        <div className="confidence-line">
          <span>Confidence {formatConfidence(analysis.confidence)}</span>
          <Progress value={Number(analysis.confidence)} />
        </div>
      ) : (
        <div className="confidence-line pending">
          <span>Confidence Pending</span>
        </div>
      )}
    </section>
  );
}

function HealthMini({ label, checks, service }) {
  const check = checks.find((item) => item.service === service);
  const status = check?.status || "unknown";
  return (
    <div className="health-mini">
      <span>{label}</span>
      <strong className={status.includes("healthy") ? "good" : status.includes("missing") ? "warn" : "bad"}>
        {status.replace("_", " ")}
      </strong>
    </div>
  );
}

function pickInitialSelection(data) {
  const replyDraft = firstReplyDraft(data);
  if (replyDraft) return { type: "reply", id: replyDraft.id };
  if (data?.blocked?.[0]) return blockedSelection(data.blocked[0]);
  if (data?.active?.[0]) return { type: "job", id: data.active[0].id };
  if (data?.queued?.[0]) return { type: "job", id: data.queued[0].id };
  if (data?.intake?.[0]) return { type: "thread", id: data.intake[0].id };
  return null;
}

function pickSectionSelection(data, section) {
  if (!data) return null;
  const replyDraft = firstReplyDraft(data);
  if (section === "review" && replyDraft) return { type: "reply", id: replyDraft.id };
  if (section === "blocked" && data.blocked?.[0]) return blockedSelection(data.blocked[0]);
  if (section === "working" && data.active?.[0]) return { type: "job", id: data.active[0].id };
  if (section === "queue" && data.queued?.[0]) return { type: "job", id: data.queued[0].id };
  if (section === "intake" && data.intake?.[0]) return { type: "thread", id: data.intake[0].id };
  if (section === "archive" && data.archive?.[0]) return { type: "thread", id: data.archive[0].id };
  return null;
}

function blockedSelection(item) {
  return { type: item?.item_type === "thread" ? "thread" : "job", id: item.id };
}

function firstReplyDraft(data) {
  return sortBySlackActivity(data?.reply_drafts)[0];
}

function sortBySlackActivity(items = []) {
  return [...(items ?? [])].sort(
    (left, right) => dateMillis(right.latest_slack_message_ts) - dateMillis(left.latest_slack_message_ts),
  );
}

function isSelectedBlocked(selected, item) {
  const next = blockedSelection(item);
  return selected?.type === next.type && selected?.id === next.id;
}

function resolveSelected(snapshot, selected) {
  if (!snapshot || !selected) return null;
  const jobs = [
    ...(snapshot.active ?? []),
    ...(snapshot.queued ?? []),
    ...(snapshot.jobs ?? []),
  ];
  const threads = snapshot.threads ?? [];
  const events = snapshot.events ?? [];
  if (selected.type === "thread") {
    const thread = [
      ...(snapshot.intake ?? []),
      ...(snapshot.archive ?? []),
      ...(snapshot.blocked ?? []).filter((item) => item.item_type === "thread"),
      ...(snapshot.threads ?? []),
    ].find((item) => item.id === selected.id);
    if (!thread) return null;
    const job = jobs.find((item) => item.intake_item_id === thread.id);
    return {
      job,
      thread,
      analysis: {
        confidence: thread.confidence,
        summary: thread.analyzer_summary,
        rationale: job?.current_block_reason || thread.analyzer_summary,
      },
      events: job ? events.filter((item) => item.job_id === job.id) : [],
    };
  }
  if (selected.type === "reply") {
    const reply = snapshot.reply_drafts?.find((item) => item.id === selected.id);
    if (!reply) return null;
    const job = jobs.find((item) => item.id === reply.job_id);
    const thread =
      threads.find((item) => item.id === reply.intake_item_id) ||
      minimalThreadFromReply(reply);
    return {
      reply,
      job,
      thread,
      analysis: {
        confidence: reply.confidence,
        summary: reply.analyzer_summary || reply.analyzer_rationale,
        rationale: reply.analyzer_rationale || reply.analyzer_summary || reply.rationale,
      },
      events: events.filter((item) => item.job_id === reply.job_id),
    };
  }
  const job = jobs.find((item) => item.id === selected.id) || snapshot.blocked?.find((item) => item.id === selected.id);
  if (!job) return null;
  const thread = threads.find((item) => item.id === job.intake_item_id) || minimalThreadFromJob(job);
  return {
    job,
    thread,
    analysis: { confidence: thread?.confidence, summary: thread?.analyzer_summary, rationale: job.current_block_reason },
    events: events.filter((item) => item.job_id === job.id),
  };
}

function minimalThreadFromReply(reply) {
  if (!reply) return null;
  return {
    id: reply.intake_item_id,
    title: reply.thread_title || reply.job_title,
    channel_id: reply.channel_id || reply.slack_channel_id,
    channel_name: reply.channel_name,
    source_type: reply.source_type,
    thread_ts: reply.thread_ts || reply.slack_thread_ts,
    permalink: reply.permalink,
    latest_slack_message_ts: reply.latest_slack_message_ts,
    trigger_display_name: reply.trigger_display_name,
    trigger_user_id: reply.trigger_user_id,
    trigger_text: reply.trigger_text,
  };
}

function minimalThreadFromJob(job) {
  if (!job) return null;
  return {
    id: job.intake_item_id,
    title: job.thread_title || job.title,
    channel_id: job.channel_id,
    channel_name: job.channel_name,
    source_type: job.source_type,
    thread_ts: job.thread_ts,
    permalink: job.permalink,
    latest_slack_message_ts: job.latest_slack_message_ts,
    trigger_display_name: job.trigger_display_name,
    trigger_user_id: job.trigger_user_id,
    trigger_text: job.trigger_text,
  };
}

function buildAnalysisView(detail) {
  const confidence = detail.analysis?.confidence ?? detail.reply?.confidence;
  const raw = [detail.analysis?.rationale, detail.analysis?.summary, detail.reply?.analyzer_summary]
    .filter(Boolean)
    .join("\n");
  const fields = uniqueAnalysisFields(parseAnalysisFields(raw));
  const field = (key) => fields.find((item) => item.key === key)?.value;
  const fallback = cleanAnalysisText(detail.analysis?.summary || detail.analysis?.rationale || detail.reply?.analyzer_summary);
  const summary =
    field("task_title") ||
    field("why_it_matters") ||
    fallback ||
    "Analyzer result will appear here.";
  return {
    confidence,
    summary,
    fields: fields.filter((item) => !["task_title", "confidence", "action_required"].includes(item.key)),
  };
}

function parseAnalysisFields(text) {
  const labels = {
    action_required: "Action",
    confidence: "Confidence",
    task_title: "Task",
    urgency: "Urgency",
    why_it_matters: "Why it matters",
    worker_plan: "Worker plan",
    needed_user_confirmation: "Needs confirmation",
  };
  const fields = [];
  let current = null;
  String(text || "")
    .replace(/\r/g, "")
    .split("\n")
    .forEach((line) => {
      const match = line.match(/^\s*([A-Za-z][A-Za-z0-9_ ]{1,48})\s*:\s*(.*)$/);
      const key = match?.[1]?.trim().toLowerCase().replaceAll(" ", "_");
      if (match && labels[key]) {
        if (current) fields.push(current);
        current = { key, label: labels[key], value: match[2].trim() };
        return;
      }
      if (current && line.trim()) {
        current.value = `${current.value} ${line.trim()}`.trim();
      }
    });
  if (current) fields.push(current);
  return fields.filter((item) => item.value && item.value !== "<nil>");
}

function uniqueAnalysisFields(fields) {
  const seen = new Set();
  return fields.filter((field) => {
    const key = `${field.key}:${field.value}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function analysisValue(fields, key) {
  return fields.find((item) => item.key === key)?.value;
}

function latestEvent(detail) {
  return detail.events?.[0]?.message;
}

function workerStatusSummary(detail) {
  if (detail.reply) return "Worker produced this draft for human review.";
  if (detail.job?.status) return `Worker state: ${statusLabel(detail.job.status)}`;
  return "No worker run is attached yet.";
}

function looksLikeIdentifier(value) {
  return String(value || "").length < 120 && /[_:0-9-]/.test(String(value || ""));
}

function cleanAnalysisText(text) {
  const value = String(text || "").trim();
  if (!value) return "";
  if (parseAnalysisFields(value).length > 0) return "";
  return value;
}

function analyzerBadgeLabel(draft) {
  const fields = parseAnalysisFields(`${draft.analyzer_rationale || ""}\n${draft.analyzer_summary || ""}`);
  const title = fields.find((item) => item.key === "task_title")?.value;
  if (!title) return "Needs action";
  if (title.length <= 18) return title;
  return `${title.slice(0, 17)}...`;
}

function slackThreadText(detail) {
  return (
    detail.message?.text ||
    detail.thread?.trigger_text ||
    detail.reply?.trigger_text ||
    detail.thread?.title ||
    detail.reply?.thread_title ||
    "Thread snapshot not loaded yet."
  );
}

function replyDraftLooksUnsafe(text) {
  const value = String(text || "").trim();
  if (!value) return false;
  const lower = value.toLowerCase();
  return [
    "slack_reply_draft:",
    "actions_taken:",
    "changed_files:",
    "blocker_reason:",
    "next_user_action:",
    "reply_drafts",
    "final reply marker",
    "structured final response",
    "worker format",
    "write directly to the database",
  ].some((signal) => lower.includes(signal));
}

function sourceLabel(value) {
  return {
    mention: "Mentioned you",
    user_participated: "My thread",
    dm: "Direct message",
  }[value] || value || "Slack";
}

function actorName(item) {
  return item?.trigger_display_name || item?.trigger_user_id || "Slack";
}

function sourceMeta(item) {
  const source = sourceLabel(item?.source_type);
  const channel = item?.channel_name || item?.channel_id;
  if (!channel || channel === source) return source;
  return `${source} / ${channel}`;
}

function slackPermalink(item) {
  const value = String(item?.permalink || "").trim();
  return value || "";
}

function slackLinkLabel(item) {
  return item?.thread_title || item?.title || item?.job_title || "Slack item";
}

function statusLabel(value) {
  return String(value || "pending").replaceAll("_", " ");
}

function detailStatus(detail) {
  if (detail.reply) return "Reply draft ready for review";
  if (detail.job?.status === "blocked") return "Blocked: needs your input";
  if (detail.thread && !detail.job) return statusLabel(detail.thread.resolution || detail.thread.status || "pending");
  return statusLabel(detail.job?.status);
}

function detailActor(detail) {
  return (
    detail.thread?.trigger_display_name ||
    detail.thread?.trigger_user_id ||
    detail.reply?.trigger_display_name ||
    detail.reply?.trigger_user_id ||
    "Slack"
  );
}

function detailSource(detail) {
  return sourceLabel(detail.thread?.source_type || detail.reply?.source_type);
}

function detailTime(detail) {
  return timeOnly(
    detail.thread?.latest_slack_message_ts ||
      detail.reply?.updated_at ||
      detail.job?.updated_at,
  );
}

function canAnalyzeThread(thread, job) {
  if (!thread || job) return false;
  return thread.status === "pending";
}

function canRunJob(job) {
  if (!job) return false;
  return ["queued", "blocked", "failed", "completed_no_reply"].includes(job.status);
}

function workerActionLabel(job) {
  if (!job) return "Run worker";
  if (job.status === "working") return "Worker running";
  if (job.status === "draft_ready") return "Draft ready";
  if (["blocked", "failed"].includes(job.status)) return "Retry worker";
  return "Run worker";
}

function healthTone(status) {
  if (String(status).includes("healthy")) return "good";
  if (String(status).includes("missing")) return "warn";
  return "neutral";
}

function formatConfidence(value) {
  if (value === null || value === undefined || value === "") return "Pending";
  const number = Number(value);
  if (!Number.isFinite(number)) return "Pending";
  return number.toFixed(2);
}

function hasConfidence(value) {
  if (value === null || value === undefined || value === "") return false;
  return Number.isFinite(Number(value));
}

function dateMillis(value) {
  if (!value) return 0;
  const date = dateFromValue(value);
  if (Number.isNaN(date.getTime())) return 0;
  return date.getTime();
}

function timeOnly(value) {
  if (!value) return "now";
  const date = dateFromValue(value);
  if (Number.isNaN(date.getTime())) return String(value).slice(11, 16) || "now";
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function displayActivityTime(value) {
  if (!value) return "N/A";
  return activityTime(value);
}

function activityTime(value) {
  if (!value) return "now";
  const date = dateFromValue(value);
  if (Number.isNaN(date.getTime())) return String(value).slice(0, 10) || "recent";
  const today = new Date();
  if (date.toDateString() === today.toDateString()) {
    return timeOnly(value);
  }
  return date.toLocaleDateString([], { month: "short", day: "numeric" });
}

function dateTimeFull(value) {
  if (!value) return "";
  const date = dateFromValue(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return date.toLocaleString([], {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

function dateFromValue(value) {
  const text = String(value || "");
  const slackMatch = text.match(/^(\d{10})(?:\.(\d{1,6}))?$/);
  if (slackMatch) {
    const seconds = Number(slackMatch[1]);
    const micros = Number((slackMatch[2] || "0").padEnd(6, "0"));
    return new Date(seconds * 1000 + Math.floor(micros / 1000));
  }
  return new Date(value);
}

function relativeTime(value) {
  if (!value) return "now";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "recent";
  const minutes = Math.max(1, Math.round((Date.now() - date.getTime()) / 60000));
  if (minutes < 60) return `${minutes}m`;
  return `${Math.round(minutes / 60)}h`;
}

function capitalize(value) {
  const text = String(value || "");
  return text.slice(0, 1).toUpperCase() + text.slice(1);
}
