import { FormEvent, useEffect, useState } from "react";

type Step = { label: string; state: string };

type Job = {
  id: string;
  title: string;
  address: string;
  status: string;
  error: string;
  runId: string;
  filename: string;
  checklist: Step[];
};

type Account = { id: string; email: string };

async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: "include", ...init });
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) {
    const message = data && typeof data.error === "string" ? data.error : res.statusText;
    throw new Error(message);
  }
  return data as T;
}

export function App() {
  const [me, setMe] = useState<Account | null>(null);
  const [ready, setReady] = useState(false);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [selected, setSelected] = useState<Job | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    api<Account>("/api/me")
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setReady(true));
  }, []);

  useEffect(() => {
    if (!me) {
      setJobs([]);
      return;
    }
    api<Job[]>("/api/jobs")
      .then(setJobs)
      .catch((err: Error) => setError(err.message));
  }, [me]);

  // While the worker runs, refresh the open sheet every second so the
  // checklist moves without a reload. The timer stops as soon as the sheet
  // leaves "running", or when another sheet is opened.
  const openId = selected?.id;
  const openStatus = selected?.status;
  useEffect(() => {
    if (!openId || openStatus !== "running") {
      return;
    }
    const timer = setInterval(() => {
      api<Job>("/api/jobs/" + openId)
        .then((job) => {
          setSelected(job);
          setJobs((current) => current.map((item) => (item.id === job.id ? job : item)));
        })
        .catch((err: Error) => setError(err.message));
    }, 1000);
    return () => clearInterval(timer);
  }, [openId, openStatus]);

  if (!ready) {
    return <p className="muted">Loading…</p>;
  }
  if (!me) {
    return <Auth onIn={setMe} />;
  }

  return (
    <main>
      <header>
        <div>
          <p className="eyebrow">Real estate</p>
          <h1>Listing board</h1>
        </div>
        <button
          className="ghost"
          onClick={() => {
            api("/api/logout", { method: "POST" }).finally(() => {
              setMe(null);
              setSelected(null);
            });
          }}
        >
          Log out
        </button>
      </header>
      {error ? <p className="error">{error}</p> : null}
      <Create
        onCreated={(job) => {
          setJobs((current) => [job, ...current]);
          setSelected(job);
          setError("");
        }}
        onError={setError}
      />
      <section className="layout">
        <ul className="list">
          {jobs.length === 0 ? <li className="muted">No sheets yet.</li> : null}
          {jobs.map((job) => (
            <li key={job.id}>
              <button className={selected?.id === job.id ? "row on" : "row"} onClick={() => setSelected(job)}>
                <span>
                  <strong>{job.title}</strong>
                  <span className="muted">{job.address}</span>
                </span>
                <span className={"status " + job.status}>{job.status}</span>
              </button>
            </li>
          ))}
        </ul>
        {selected ? (
          <Sheet
            job={selected}
            onChange={(job) => {
              setSelected(job);
              setJobs((current) => current.map((item) => (item.id === job.id ? job : item)));
            }}
            onError={setError}
          />
        ) : (
          <p className="muted">Select a sheet.</p>
        )}
      </section>
    </main>
  );
}

function Auth({ onIn }: { onIn: (account: Account) => void }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    setError("");
    try {
      const account = await api<Account>("/api/" + mode, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password }),
      });
      onIn(account);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not sign in");
    }
  }

  return (
    <main className="narrow">
      <p className="eyebrow">Real estate</p>
      <h1>Listing board</h1>
      <form onSubmit={submit}>
        <label>
          Email
          <input value={email} onChange={(event) => setEmail(event.target.value)} type="email" required />
        </label>
        <label>
          Password
          <input value={password} onChange={(event) => setPassword(event.target.value)} type="password" required />
        </label>
        {error ? <p className="error">{error}</p> : null}
        <button type="submit">{mode === "login" ? "Sign in" : "Create account"}</button>
      </form>
      <button className="ghost" onClick={() => setMode(mode === "login" ? "register" : "login")}>
        {mode === "login" ? "Need an account?" : "Already have an account?"}
      </button>
    </main>
  );
}

function Create({ onCreated, onError }: { onCreated: (job: Job) => void; onError: (message: string) => void }) {
  const [title, setTitle] = useState("");
  const [address, setAddress] = useState("");
  const [file, setFile] = useState<File | null>(null);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!file) {
      onError("Choose a brochure file");
      return;
    }
    try {
      const job = await api<Job>("/api/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ title, address }),
      });
      const body = new FormData();
      body.append("file", file);
      const saved = await api<Job>("/api/jobs/" + job.id + "/file", { method: "POST", body });
      setTitle("");
      setAddress("");
      setFile(null);
      onCreated(saved);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not create the sheet");
    }
  }

  return (
    <form className="create" onSubmit={submit}>
      <label>
        Title
        <input value={title} onChange={(event) => setTitle(event.target.value)} required />
      </label>
      <label>
        Address
        <input value={address} onChange={(event) => setAddress(event.target.value)} required />
      </label>
      <label>
        Brochure
        <input
          type="file"
          accept=".pdf,.txt,application/pdf,text/plain"
          onChange={(event) => setFile(event.target.files?.[0] ?? null)}
          required
        />
      </label>
      <button type="submit">Create sheet</button>
    </form>
  );
}

function Sheet({
  job,
  onChange,
  onError,
}: {
  job: Job;
  onChange: (job: Job) => void;
  onError: (message: string) => void;
}) {
  const [starting, setStarting] = useState(false);

  async function start() {
    onError("");
    setStarting(true);
    try {
      const next = await api<Job>("/api/jobs/" + job.id + "/start", { method: "POST" });
      onChange(next);
    } catch (err) {
      onError(err instanceof Error ? err.message : "Could not start the sheet");
    } finally {
      setStarting(false);
    }
  }

  // The worker marks steps done one at a time; the page polls while the sheet
  // runs (see App), so this view only has to draw what the sheet says.
  const done = job.checklist.filter((step) => step.state === "done").length;
  const active = job.status === "running" ? job.checklist.findIndex((step) => step.state === "pending") : -1;
  const progress = job.checklist.length ? (done / job.checklist.length) * 100 : 0;

  return (
    <article className="sheet">
      <header className="sheet-head">
        <div>
          <h2>{job.title}</h2>
          <p className="muted">{job.address}</p>
        </div>
        <span className={"status " + job.status}>{job.status}</span>
      </header>
      {job.filename ? <p className="file">{job.filename}</p> : null}
      {job.error ? <p className="error">{job.error}</p> : null}
      <div className="track" aria-hidden="true">
        <div className="track-fill" style={{ width: progress + "%" }} />
      </div>
      <ol className="steps" aria-label="Checklist">
        {job.checklist.map((step, index) => {
          const state = step.state === "done" ? "done" : index === active ? "active" : "pending";
          return (
            <li key={step.label} className={"step " + state}>
              <span className="mark" aria-hidden="true">
                {state === "done" ? "\u2713" : state === "active" ? "" : index + 1}
              </span>
              <span className="step-label">{step.label}</span>
              {state === "active" ? <span className="sr-only">in progress</span> : null}
            </li>
          );
        })}
      </ol>
      {job.status === "draft" ? (
        <p className="muted">
          {job.filename ? "Ready to run. Start walks the sheet through the three steps." : "Attach a brochure, then start the sheet."}
        </p>
      ) : null}
      {job.status === "draft" ? (
        <button onClick={start} disabled={starting}>
          {starting ? "Starting\u2026" : "Start"}
        </button>
      ) : null}
      {job.status === "running" ? <p className="muted">The worker is on it. This page updates by itself.</p> : null}
      {job.status === "success" ? <p className="done-note">All three steps are done. The sheet is published.</p> : null}
    </article>
  );
}
