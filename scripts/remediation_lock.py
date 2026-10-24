"""Share one repair owner across daily and bounded Ghost tasks on this computer.

The OS lock protects the ownership transaction. The durable owner is the actual
Ghost run, so a short helper process exiting does not release the repair lease.
Only authoritative terminal run evidence permits orphan recovery.
"""
import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import tempfile

from review_cloudwatch import aws_call, ReadFailure

TERMINAL = {"COMPLETED", "EXITED", "FAILED", "ERROR", "CANCELLED"}
REPO = "JeremyProffittOrg/live-ninja"


def terminal(row):
    observed = row.get("observation_terminal", {}).get("BOOL")
    if isinstance(observed, bool):
        return observed
    return row.get("status", {}).get("S") in TERMINAL


def get_row(table, key_name, value, projection, names=None):
    params = {"TableName": table, "Key": {key_name: {"S": value}}, "ConsistentRead": True,
              "ProjectionExpression": projection}
    if names:
        params["ExpressionAttributeNames"] = names
    row = aws_call("us-east-1", "get-item", params, 40, service="dynamodb").get("Item")
    if not row:
        raise ReadFailure("ghost_run_metadata_missing")
    return row


def event_run(event_id):
    event = get_row("ghost-cli-scheduled-events", "event_id", event_id,
                    "event_id,#repo,#node,last_run_id", {"#repo": "repo", "#node": "node"})
    if event.get("repo", {}).get("S") != REPO or event.get("node", {}).get("S") != "OFFICEPC":
        raise ReadFailure("wrong_ghost_task")
    run_id = event.get("last_run_id", {}).get("S")
    if not run_id:
        raise ReadFailure("no_active_ghost_run")
    run = run_state(run_id)
    if run.get("event_id", {}).get("S") != event_id or run.get("node", {}).get("S") != "OFFICEPC":
        raise ReadFailure("wrong_ghost_run_binding")
    if terminal(run):
        raise ReadFailure("no_active_ghost_run")
    return run_id


def run_state(run_id):
    return get_row("ghost-cli-scheduled-runs", "run_id", run_id, "#status,#node,event_id,observation_terminal", {"#status": "status", "#node": "node"})


@contextmanager
def guard(root):
    root.mkdir(parents=True, exist_ok=True)
    with (root / "guard.lock").open("a+b") as stream:
        stream.seek(0, 2)
        if stream.tell() == 0:
            stream.write(b"0")
            stream.flush()
        stream.seek(0)
        try:
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            raise ReadFailure("lock_transaction_busy") from None
        try:
            yield
        finally:
            stream.seek(0)
            if os.name == "nt":
                msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                fcntl.flock(stream.fileno(), fcntl.LOCK_UN)


def acquire(root, event_id, run_id, read_run=run_state):
    with guard(root):
        owner_file = root / "owner.json"
        if owner_file.exists():
            try:
                owner = json.loads(owner_file.read_text(encoding="utf-8"))
                previous = owner["run_id"]
            except (ValueError, KeyError, TypeError):
                raise ReadFailure("unverifiable_lock_owner") from None
            if previous == run_id and owner.get("event_id") == event_id:
                return owner
            if not terminal(read_run(previous)):
                raise ReadFailure("another_remediation_run_is_active")
        owner = {"event_id": event_id, "run_id": run_id}
        temporary = root / "owner.tmp"
        temporary.write_text(json.dumps(owner), encoding="utf-8")
        temporary.replace(owner_file)
        return owner


def release(root, event_id, run_id):
    with guard(root):
        path = root / "owner.json"
        if not path.exists():
            return
        owner = json.loads(path.read_text(encoding="utf-8"))
        if owner != {"event_id": event_id, "run_id": run_id}:
            raise ReadFailure("lock_belongs_to_another_run")
        path.unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["acquire", "release"])
    parser.add_argument("--event-id", required=True)
    parser.add_argument("--run-id")
    args = parser.parse_args()
    base = Path(os.environ.get("LOCALAPPDATA", tempfile.gettempdir())) / "LiveNinja" / "remediation"
    try:
        if args.action == "acquire":
            identity = aws_call("us-east-1", "get-caller-identity", {}, 40, service="sts")
            if identity.get("Account") != "759775734231":
                raise ReadFailure("wrong_aws_account")
            run_id = event_run(args.event_id)
            print(json.dumps({"status": "ACQUIRED", **acquire(base, args.event_id, run_id)}))
        else:
            if not args.run_id:
                parser.error("release requires the run ID returned by acquire")
            release(base, args.event_id, args.run_id)
            print(json.dumps({"status": "RELEASED"}))
        return 0
    except ReadFailure as error:
        print(json.dumps({"status": "BLOCKED", "code": str(error)}))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
