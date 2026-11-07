// The account's record as a timeline: its creation, every write to it and every
// step the setup reported, with the time, the source and the result. A source
// only says where a change came from; the page never calls it an approval.
import { useEffect, useState } from "react";
import { formatClock, formatUntil } from "../lib/format";
import {
  readRecord,
  sourceText,
  timelineOf,
  type AccountRecord,
  type RecordOutcome,
  type TimelineItem,
} from "../lib/record";
import {
  auditActionWording,
  recordErrorWording,
  setupReasonWording,
  setupStateWording,
  setupStepWording,
} from "../lib/words";
import { Empty, Status, Time } from "./ui";

/** What a line of the timeline says happened, as a phrase. */
function Result({ x }: { x: TimelineItem }) {
  if (x.kind === "created") return <>Created</>;
  if (x.kind === "report" || x.kind === "outcome") {
    const w = setupStateWording(x.state);
    return (
      <>
        <Status state={w} />
        {x.step === "" ? null : <> {setupStepWording(x.step)}</>}
        {x.reason === "" ? null : (
          <span className="muted"> · {setupReasonWording(x.reason)}</span>
        )}
        {x.wait_until === "" ? null : (
          <span className="muted">
            {" "}
            · until {formatClock(x.wait_until)} UTC
          </span>
        )}
      </>
    );
  }
  return <>{auditActionWording(x.action)}</>;
}

/** Where the latest task stands, and the CAPTCHA deadline while it waits. */
function Latest({ r, now }: { r: AccountRecord; now: Date }) {
  const s = r.setup;
  if (s === undefined || s === null || s.state === "not_started") return null;
  const w = setupStateWording(s.state);
  const waiting =
    s.state === "running" &&
    s.step === "awaiting_captcha" &&
    s.wait_until !== undefined &&
    s.wait_until !== null;
  return (
    <div className="panel">
      <p>
        <Status state={w} />
        {s.step === null ? null : <> at: {setupStepWording(s.step)}</>}
        {s.reason === null ? null : (
          <span className="muted"> · {setupReasonWording(s.reason)}</span>
        )}
      </p>
      {waiting ? (
        <p className="alert" role="status">
          Waiting for a person to pass the CAPTCHA; times out at{" "}
          <Time iso={s.wait_until ?? ""}>
            {formatClock(s.wait_until ?? "")} UTC
          </Time>{" "}
          <span className="muted">
            ({formatUntil(s.wait_until ?? "", now)})
          </span>
        </p>
      ) : null}
    </div>
  );
}

/** The timeline of a record already read. */
export function TimelineView({
  record,
  now,
}: {
  record: AccountRecord;
  now: Date;
}) {
  const items = timelineOf(record);
  return (
    <>
      <Latest r={record} now={now} />
      {items.length === 0 ? (
        <Empty title="No record yet">
          The gateway has written nothing for this account.
        </Empty>
      ) : (
        <div className="table-wrap scroll">
          <table>
            <thead>
              <tr>
                <th>Time</th>
                <th>What</th>
                <th>Source</th>
              </tr>
            </thead>
            <tbody>
              {items.map((x, i) => (
                <tr key={`${x.at}-${x.action}-${String(i)}`}>
                  <td>
                    <Time iso={x.at}>
                      {x.at.slice(0, 19).replace("T", " ")}
                    </Time>
                  </td>
                  <td>
                    <Result x={x} />
                  </td>
                  <td className="mono small">{sourceText(x)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {record.history_truncated ? (
        <p className="small muted">
          Only the latest rows are shown; the creation row is kept apart.
        </p>
      ) : null}
    </>
  );
}

/** Reads the record of alias again whenever stamp changes. */
export function Timeline({
  alias,
  stamp,
  now,
}: {
  alias: string;
  stamp: string;
  now: Date;
}) {
  const [outcome, setOutcome] = useState<RecordOutcome | null>(null);
  useEffect(() => {
    let live = true;
    void readRecord(alias).then((o) => {
      if (live) setOutcome(o);
    });
    return () => {
      live = false;
    };
  }, [alias, stamp]);
  if (outcome === null)
    return <p className="small muted">Reading the record…</p>;
  if (!outcome.ok)
    return (
      <p className="small muted">
        The record could not be read: {recordErrorWording(outcome.text)}
      </p>
    );
  return <TimelineView record={outcome.record} now={now} />;
}
