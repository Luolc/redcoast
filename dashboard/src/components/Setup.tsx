// The setup of an account's token: the button that asks the gateway for it,
// the state of the latest task, and when the token was set and falls due.
// The page never takes or shows a token, only states, codes and times.
import { useState } from "react";
import {
  requestSetup,
  setupBusy,
  setupOffReason,
  type Managed,
} from "../lib/admin";
import { type Refusal } from "../lib/adminApi";
import { routeHref } from "../lib/route";
import type { Dashboard } from "../lib/types";
import {
  setupReasonWording,
  setupRefusalWording,
  setupStateWording,
  setupStepWording,
  tokenExpiryWording,
} from "../lib/words";
import { Confirm, RefusalCallout } from "./AdminUi";
import { Ago, Status, Time } from "./ui";

function wordedRefusal(r: Refusal): string {
  const sentence = setupRefusalWording(r.text);
  return sentence === r.text ? r.text : `${sentence} (${r.text})`;
}

function StateBlock({ m, now }: { m: Managed; now: Date }) {
  const s = m.config?.setup;
  if (s === undefined) return null;
  const w = setupStateWording(s.state);
  return (
    <>
      <dl className="facts">
        <div className="fact">
          <dt>Setup</dt>
          <dd>
            <Status state={w} />
          </dd>
        </div>
        {s.step === null ? null : (
          <div className="fact">
            <dt>Step</dt>
            <dd>
              {setupStepWording(s.step)}{" "}
              <span className="mono faint">{s.step}</span>
            </dd>
          </div>
        )}
        {s.reason === null ? null : (
          <div className="fact">
            <dt>Reason</dt>
            <dd>
              {setupReasonWording(s.reason)}{" "}
              <span className="mono faint">{s.reason}</span>
            </dd>
          </div>
        )}
        {s.updated_at === null ? null : (
          <div className="fact">
            <dt>Updated</dt>
            <dd className="muted">
              <Ago iso={s.updated_at} now={now} />
            </dd>
          </div>
        )}
      </dl>
      {s.state === "needs_human" ? (
        <div className="alert" role="status">
          A person has to do this by hand: log the account in, put its token in
          the secret manager at the reference above, then press Verify (step 3
          of{" "}
          <a href={routeHref({ view: "add", arg: `3/${m.alias}` })}>
            adding an account
          </a>
          ).
        </div>
      ) : null}
    </>
  );
}

/** The setup button, or the sentence that says why there is none, and the state. */
export function SetupPanel({
  data,
  m,
  now,
  reload,
}: {
  data: Dashboard;
  m: Managed;
  now: Date;
  reload: () => void;
}) {
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);
  const c = m.config;
  if (c === undefined) return null;
  const off = setupOffReason(data, m);
  const open = setupBusy(m);
  const ask = () => {
    setBusy(true);
    void requestSetup(m.alias, c.rev).then((r) => {
      setBusy(false);
      if (r.ok) {
        setRefusal(null);
        reload();
      } else
        setRefusal({
          ...r.refusal,
          text: wordedRefusal(r.refusal),
        });
    });
  };
  return (
    <div className="panel">
      <StateBlock m={m} now={now} />
      {refusal === null ? null : (
        <RefusalCallout
          title="Not requested"
          refusal={refusal}
          onRefresh={reload}
        />
      )}
      {off !== null ? (
        <p className="small muted">{off}</p>
      ) : open ? (
        <p className="small muted">
          A setup is under way; the button returns when it ends.
        </p>
      ) : (
        <div className="form">
          <p className="small muted">
            The gateway queues the login for a login machine; this page never
            sees the token.
          </p>
          <div className="form-foot">
            <Confirm
              key={refusal === null ? "ask" : "refused"}
              label="Start setup"
              primary
              busy={busy}
              yes="Yes, start setup"
              question={
                <>
                  Log {m.alias} in and set its token? A login machine takes it
                  up within a minute or so, and the token it makes replaces the
                  one at the reference.
                </>
              }
              onYes={ask}
            />
          </div>
        </div>
      )}
    </div>
  );
}

/** When the gateway wrote the token and the expiry its item carries. */
export function TokenDates({ m }: { m: Managed }) {
  const setup = m.config?.setup;
  const off = <span className="faint">setup is off</span>;
  const set = setup?.token_set_at;
  const expiry = setup?.token_expiry;
  return (
    <>
      <div className="fact">
        <dt>Token set</dt>
        <dd>
          {setup === undefined ? (
            off
          ) : set === null || set === undefined ? (
            <span className="faint">not recorded</span>
          ) : (
            <Time iso={set}>{set.slice(0, 10)}</Time>
          )}
        </dd>
      </div>
      <div className="fact">
        <dt>Token expires</dt>
        <dd>
          {setup === undefined ? (
            off
          ) : expiry === undefined ? (
            <span className="faint">not recorded</span>
          ) : expiry.state === "read" && expiry.expires_at !== null ? (
            <Time iso={expiry.expires_at}>
              {expiry.expires_at.slice(0, 10)}
            </Time>
          ) : (
            <span className="muted">{tokenExpiryWording(expiry.state)}</span>
          )}
        </dd>
      </div>
    </>
  );
}
