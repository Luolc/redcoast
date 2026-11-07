// Parts the account pages share: form fields, the two-step confirm, the
// step bar, a credential reference, the exhaustion estimate and the checks.
import { Check, Lock } from "lucide-react";
import { useId, useState, type ReactNode } from "react";
import { isConflict, type Refusal, type VerifyReport } from "../lib/adminApi";
import { formatSpan, formatUntil } from "../lib/format";
import type { ProxyRefOption, Verification, WindowBurn } from "../lib/types";
import {
  burnReason,
  checkWording,
  CHECK_NAMES,
  refusedName,
} from "../lib/words";
import { Ago, Status, Time } from "./ui";

export function TextField({
  label,
  value,
  onChange,
  hint,
  error,
  mono = false,
  readOnly = false,
  placeholder,
  numeric = false,
}: {
  label: string;
  value: string;
  onChange?: (v: string) => void;
  hint?: ReactNode;
  error?: string | null | undefined;
  mono?: boolean;
  readOnly?: boolean;
  placeholder?: string;
  numeric?: boolean;
}) {
  const id = useId();
  const note = `${id}-note`;
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        className={`input${mono ? " mono" : ""}${numeric ? " num" : ""}`}
        value={value}
        readOnly={readOnly}
        placeholder={placeholder}
        spellCheck={false}
        autoComplete="off"
        aria-invalid={error === undefined || error === null ? undefined : true}
        aria-describedby={note}
        onChange={(e) => onChange?.(e.target.value)}
      />
      <div
        id={note}
        className={`hint${error === undefined || error === null || error === "" ? "" : " s-bad"}`}
      >
        {error ?? hint}
      </div>
    </div>
  );
}

export function SelectField({
  label,
  value,
  options,
  onChange,
  hint,
}: {
  label: string;
  value: string;
  options: { value: string; label: string }[];
  onChange: (v: string) => void;
  hint?: ReactNode;
}) {
  const id = useId();
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <select
        id={id}
        className="input"
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
        }}
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      <div className="hint">{hint}</div>
    </div>
  );
}

/**
 * An act that takes a second click: the first asks, and names what will
 * happen; Keep goes back.
 */
export function Confirm({
  label,
  question,
  yes,
  onYes,
  disabled = false,
  primary = false,
  busy = false,
  icon,
}: {
  label: string;
  question: ReactNode;
  yes: string;
  onYes: () => void;
  disabled?: boolean;
  primary?: boolean;
  busy?: boolean;
  icon?: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  if (!open)
    return (
      <button
        type="button"
        className={`btn${primary ? " primary" : ""}`}
        disabled={disabled}
        onClick={() => {
          setOpen(true);
        }}
      >
        {icon}
        {label}
      </button>
    );
  return (
    <div className="ask" role="group" aria-label={label}>
      <div className="q">{question}</div>
      <div className="acts">
        <button
          type="button"
          className="btn primary"
          disabled={busy}
          onClick={onYes}
        >
          {yes}
        </button>
        <button
          type="button"
          className="btn"
          disabled={busy}
          onClick={() => {
            setOpen(false);
          }}
        >
          Keep
        </button>
      </div>
    </div>
  );
}

const STEPS = ["Fill in", "Saved", "Log in", "Account details", "Activate"];

/** The five steps of adding an account, with the one in hand marked. */
export function Stepper({ step }: { step: number }) {
  return (
    <ol className="stepper" aria-label="Steps of adding an account">
      {STEPS.map((name, i) => {
        const n = i + 1;
        const state = n < step ? "done" : n === step ? "now" : "next";
        return (
          <li
            key={name}
            className={`st-${state}`}
            aria-current={state === "now" ? "step" : undefined}
          >
            <span className="mark">
              {state === "done" ? (
                <Check className="icon sm" aria-hidden="true" />
              ) : (
                n
              )}
            </span>
            <span className="name">{name}</span>
          </li>
        );
      })}
    </ol>
  );
}

/** A credential's reference, never its value. */
export function RefLine({
  label,
  reference,
}: {
  label: string;
  reference: string;
}) {
  return (
    <div className="ref">
      <div className="k">
        <Lock className="icon sm" aria-hidden="true" />
        {label}
      </div>
      <div className="mono small ref-v">{reference}</div>
    </div>
  );
}

/**
 * The gateway's estimate of one window: its speed, when it is full at that
 * speed, its reset, and whether it runs out first. Compact is one line, for
 * a table cell. With too few readings it says why.
 */
export function Estimate({
  burn: b,
  now,
  compact = false,
}: {
  burn: WindowBurn | undefined;
  now: Date;
  compact?: boolean;
}) {
  if (b === undefined) return <span className="muted small">No estimate</span>;
  if (b.rate_per_hour === null || b.exhausts_at === null)
    return (
      <span className="muted small">No estimate: {burnReason(b.reason)}</span>
    );
  const rate = `${(b.rate_per_hour * 100).toFixed(b.rate_per_hour < 0.1 ? 1 : 0)} %/h`;
  const full = formatUntil(b.exhausts_at, now).replace("in ", "");
  const before = b.before_reset === true;
  const tone = before ? "s-bad" : "s-ok";
  const hint =
    "Estimate: a line through the readings since the last reset, run on to full";
  if (compact)
    return (
      <span className={`${tone} small`} title={hint}>
        Est. {rate} · {before ? `out in ${full}` : "lasts to reset"}
      </span>
    );
  return (
    <dl className="est" title={hint}>
      <div>
        <dt>Speed</dt>
        <dd className="num">{rate}</dd>
      </div>
      <div>
        <dt>Full at this speed</dt>
        <dd>
          in <span className="num">{full}</span>
          {" · "}
          <Time iso={b.exhausts_at}>{b.exhausts_at.slice(11, 16)} UTC</Time>
        </dd>
      </div>
      <div>
        <dt>Resets</dt>
        <dd>
          {b.resets_at === null ? (
            "unknown"
          ) : (
            <Time iso={b.resets_at}>{formatUntil(b.resets_at, now)}</Time>
          )}
        </dd>
      </div>
      <div>
        <dt>Before the reset</dt>
        <dd className={tone}>
          {before ? "Runs out before reset" : "Lasts until reset"}
        </dd>
      </div>
      <div className="faint small">
        Estimate from {String(b.points)} readings over{" "}
        {formatSpan(b.span_minutes * 60_000)}
      </div>
    </dl>
  );
}

/** Why the page cannot write. */
export function WriteNotice({ reason }: { reason: string }) {
  return (
    <span className="small s-warn" role="status">
      {reason}
    </span>
  );
}

const CHECKS = ["token_ok", "egress_ok", "probe_ok"] as const;

/**
 * The three checks, each with its result. The last run is stored as passes
 * and failures; a report from the run just made adds the reason of each.
 */
export function ChecksTable({
  verify,
  report,
  now,
}: {
  verify: Verification;
  report?: VerifyReport | undefined;
  now: Date;
}) {
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th scope="col">Check</th>
            <th scope="col">Result</th>
            <th scope="col">Run</th>
            <th scope="col">Detail</th>
          </tr>
        </thead>
        <tbody>
          {CHECKS.map((k) => {
            const r = report?.[k];
            const state =
              r?.result === "skipped"
                ? { tone: "off" as const, word: "Skipped" }
                : checkWording(verify[k]);
            return (
              <tr key={k}>
                <td>
                  {CHECK_NAMES[k]} <span className="mono faint small">{k}</span>
                </td>
                <td>
                  <Status state={state} />
                </td>
                <td className="muted">
                  {verify.at === undefined || verify[k] === null ? (
                    "–"
                  ) : (
                    <Ago iso={verify.at} now={now} />
                  )}
                </td>
                <td className="small muted">{r?.reason ?? ""}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** The proxy login of an account: one of the pairs accounts already use. */
export function ProxyLoginField({
  value,
  options,
  onChange,
}: {
  value: string;
  options: ProxyRefOption[];
  onChange: (index: string) => void;
}) {
  return (
    <SelectField
      label="Proxy login"
      value={value}
      onChange={onChange}
      options={options.map((o, i) => ({
        value: String(i),
        label: o.username_ref.replace(/\/username$/, "/{username,password}"),
      }))}
      hint="Only a login that accounts already use; a new one is added at the command line"
    />
  );
}

/** A refusal of a write: the sentence, and what it concerns. */
export function RefusalCallout({
  title,
  refusal,
  onRefresh,
}: {
  title: string;
  refusal: Refusal;
  onRefresh?: () => void;
}) {
  const concerns = refusal.check ?? refusal.field;
  return (
    <div role="alert" className="callout bad">
      <Status state={{ tone: "bad", word: title }} /> {refusal.text}
      {concerns === undefined ? null : (
        <div className="small">
          Concerns: {refusedName(concerns)}{" "}
          <span className="mono faint">{concerns}</span>
        </div>
      )}
      {isConflict(refusal) && onRefresh !== undefined ? (
        <div className="acts">
          Someone changed this account since this page read it. Refresh to read
          the new values, then make your change again.{" "}
          <button type="button" className="btn" onClick={onRefresh}>
            Refresh
          </button>
        </div>
      ) : null}
    </div>
  );
}
