"""Onebox durable executions v1. Invoked on demand, never a resident service."""

import base64
import contextlib
import fcntl
import hashlib
import json
import math
import os
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

VERSION = 1
LIMIT = 4 * 1024 * 1024
ID = re.compile(r"[0-9a-f]{32}")
RELEASE = re.compile(r"[0-9]{8}-[0-9]{6}-[0-9A-Za-z_-]+")
TERMINAL = {"succeeded", "abandoned"}


def require(ok, message):
    if not ok:
        raise ValueError(message)


def encode(value):
    return json.dumps(
        value, sort_keys=True, separators=(",", ":"), ensure_ascii=True
    ).encode()


def digest(value):
    return hashlib.sha256(encode(value)).hexdigest()


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def directory(path):
    try:
        path.mkdir(mode=0o700)
        fsync_dir(path.parent)
    except FileExistsError:
        require(
            stat.S_ISDIR(path.lstat().st_mode),
            "state directory is not a real directory",
        )


def read_json(path, limit=LIMIT):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as stream:
        require(
            stat.S_ISREG(os.fstat(stream.fileno()).st_mode),
            "state/output is not a regular file",
        )
        data = stream.read(limit + 1)
    require(len(data) <= limit, "state/output exceeds its size limit")
    return json.loads(
        data, object_pairs_hook=unique_object, parse_constant=invalid_constant
    )


def invalid_constant(_value):
    raise ValueError("non-finite JSON number")


def unique_object(pairs):
    obj = {}
    for key, value in pairs:
        require(key not in obj, "duplicate JSON key")
        obj[key] = value
    return obj


def atomic(path, value):
    data = encode(value)
    require(
        len(data) <= LIMIT,
        "execution history limit reached; abandon and start a new execution",
    )
    atomic_bytes(path, data)


def atomic_bytes(path, data):
    fd, tmp = tempfile.mkstemp(prefix=".checkpoint-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(tmp, path)
        fsync_dir(path.parent)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


class Store:
    def __init__(self, root):
        self.root = Path(root)
        self.path = self.root / "schedule" / "executions"

    def record_path(self, identity):
        require(ID.fullmatch(identity), "invalid execution ID")
        return self.path / (identity + ".json")

    @contextlib.contextmanager
    def locked(self):
        directory(self.path.parent)
        directory(self.path)
        fd = os.open(self.path / ".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX)
            yield
        finally:
            os.close(fd)

    def read(self, identity):
        value = read_json(self.record_path(identity))
        require(
            value.get("version") == VERSION and value.get("id") == identity,
            "unsupported or corrupt execution record",
        )
        require(RELEASE.fullmatch(value["release"]), "invalid release evidence")
        require(
            value["definition_digest"] == digest(value["definition"]),
            "execution definition changed",
        )
        require(
            value["state"]
            in TERMINAL | {"pending", "running", "failed", "interrupted", "timeout"},
            "invalid execution state",
        )
        require(
            type(value["expires_at"]) in (int, float)
            and math.isfinite(value["expires_at"]),
            "invalid execution expiry",
        )
        require(
            len(value["steps"]) == len(value["definition"]["steps"]),
            "invalid step evidence",
        )
        for step, definition in zip(value["steps"], value["definition"]["steps"]):
            require(
                step["name"] == definition["name"]
                and step["id"] == identity + ":" + step["name"],
                "invalid step identity",
            )
            require(
                step["state"]
                in {"pending", "running", "succeeded", "failed", "interrupted"},
                "invalid step state",
            )
            if step["state"] == "succeeded":
                validate_outputs(step["outputs"], definition["outputs"])
        return value

    def write(self, value):
        value["updated_at"] = time.time()
        atomic(self.record_path(value["id"]), value)

    def records(self):
        if not os.path.lexists(self.path):
            return []
        require(stat.S_ISDIR(self.path.lstat().st_mode), "invalid execution directory")
        return [self.read(p.stem) for p in sorted(self.path.glob("*.json"))]

    def prune(self):
        # Caller holds the store lock. Never age out uncertain running work:
        # only an operator can establish it is safe to abandon after a crash.
        records = self.records()
        changed = False
        for value in records:
            if value["state"] != "running" and time.time() >= value["expires_at"]:
                self.record_path(value["id"]).unlink()
                changed = True
        if changed:
            fsync_dir(self.path)


class ImageNotFound(ValueError):
    pass


def docker(args, capture=True):
    result = subprocess.run(
        ["/usr/bin/docker"] + args,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
        check=False,
    )
    if result.returncode != 0 and capture and args[:2] == ["image", "inspect"]:
        # Only Docker's explicit missing-image diagnostic permits a pull.
        # Unknown errors fail closed; never expose raw stderr or credentials.
        diagnostic = result.stderr.decode(errors="replace").strip()
        if (
            diagnostic.startswith(
                (
                    "Error: No such image: ",
                    "Error response from daemon: No such image: ",
                )
            )
            and "\n" not in diagnostic
        ):
            raise ImageNotFound("job image is not locally available")
        require(
            False,
            "Docker image inspection failed; check Docker daemon access and permissions",
        )
    # Docker errors may contain expanded secret configuration. Keep them in the
    # operator's container logs, never in durable public state or error metadata.
    require(result.returncode == 0, "Docker operation failed: " + args[0])
    return result.stdout.decode() if capture else ""


def container_running(name):
    ids = docker(["ps", "-aq", "--filter", "name=^/" + name + "$"]).split()
    if not ids:
        return False
    rows = json.loads(docker(["inspect"] + ids))
    return any(
        row["State"].get("Running") or row["State"].get("Restarting") for row in rows
    )


def cleanup_container(config, invocation=None):
    ids = docker(
        ["ps", "-aq", "--filter", "name=^/" + config["container"] + "$"]
    ).split()
    if not ids:
        return
    rows = json.loads(docker(["inspect"] + ids))
    for row in rows:
        labels = row["Config"].get("Labels") or {}
        legacy_job = (
            invocation is None
            and not any(key.startswith("ob.execution.") for key in labels)
            and row.get("Name") == "/" + config["container"]
            and labels.get("com.docker.compose.project") == config["application"]
            and labels.get("com.docker.compose.service") == config["job"]
            and labels.get("com.docker.compose.oneoff", "").lower() == "true"
        )
        require(
            labels.get("ob.execution.job") == config["job"] or legacy_job,
            "existing container has no matching job ownership; inspect and remove it manually",
        )
        if invocation is None:
            require(
                not (row["State"].get("Running") or row["State"].get("Restarting")),
                "previous container is still running",
            )
        else:
            require(
                labels.get("ob.execution.invocation") == invocation,
                "container belongs to another invocation",
            )
        # Pre-attempt cleanup never forces removal: Docker must also refuse if
        # the stopped container starts after our inspection.
        docker(["rm"] + (["-f"] if invocation is not None else []) + [row["Id"]])


def compatibility(config, release_dir):
    # Only public container identities are retained; never docker inspect's env.
    services = []
    for name in config["services"]:
        rows = json.loads(docker(["inspect", name]))
        require(
            len(rows) == 1 and rows[0]["State"]["Running"],
            "required service is not running",
        )
        services.append(
            [name, rows[0]["Id"], rows[0]["Image"], rows[0]["State"]["StartedAt"]]
        )
    files = []
    for relative in (
        ["compose.yaml"] + config["env_files"] + (config.get("fingerprint_files") or [])
    ):
        path = release_dir / relative
        require(path.is_file(), "required release file is missing")
        files.append([relative, hashlib.sha256(path.read_bytes()).hexdigest()])
    generation = (
        release_dir.parent.parent / "schedule" / "executions" / ".data-generation"
    )
    data_generation = read_json(generation) if os.path.lexists(generation) else ""
    return digest([services, files, data_generation])


def compose(config, release_dir):
    args = [
        "compose",
        "--profile",
        "job",
        "-p",
        config["application"],
        "--project-directory",
        str(release_dir),
        "-f",
        str(release_dir / "compose.yaml"),
    ]
    for file in config["env_files"]:
        args += ["--env-file", str(release_dir / file)]
    return args


def image_identity(config, release_dir, allow_pull=False):
    # Never persist the expanded compose config: it can contain credentials.
    model = json.loads(
        docker(compose(config, release_dir) + ["config", "--format", "json"])
    )
    reference = model["services"][config["job"]]["image"]
    try:
        rows = json.loads(docker(["image", "inspect", reference]))
    except ImageNotFound:
        require(allow_pull, "original job image is not locally available")
        docker(["pull", reference])
        rows = json.loads(docker(["image", "inspect", reference]))
    require(
        len(rows) == 1 and re.fullmatch(r"sha256:[0-9a-f]{64}", rows[0]["Id"]),
        "job image is not locally available with an immutable identity",
    )
    return rows[0]["Id"]


def prepare(store, config, release, invocation, resume, operation, overrides):
    require(
        ID.fullmatch(invocation), "durable execution requires a systemd invocation ID"
    )
    require(RELEASE.fullmatch(release), "invalid starting release")
    release_dir = store.root / "releases" / release
    require(
        (store.root / "current").resolve() == release_dir.resolve(),
        "current release changed",
    )
    require(
        not container_running(config["container"]),
        "previous job container is still running; stop it before retrying",
    )
    identity = image_identity(config, release_dir, allow_pull=not resume)
    observed = compatibility(config, release_dir)
    with store.locked():
        if resume:
            value = store.read(resume)
            require(value["state"] not in TERMINAL, "execution is terminal")
            require(
                time.time() < value["expires_at"], "execution has expired; abandon it"
            )
            require(
                value["release"] == release,
                "resume requires the original current release",
            )
            require(
                value["definition_digest"] == digest(config),
                "installed workflow definition changed",
            )
            require(
                value["compatibility"] == observed,
                "service identities or release files changed",
            )
            require(value["image"] == identity, "job image changed or is unavailable")
            require(not overrides, "resume cannot override original inputs")
            if value["activations"][-1]["state"] == "running":
                value["activations"][-1]["state"] = "interrupted"
            for step in value["steps"]:
                if step["state"] == "running":
                    step["state"] = "interrupted"
                    step["attempts"][-1]["state"] = "interrupted"
        else:
            store.prune()
            inputs = dict(config["defaults"])
            require(set(overrides) <= set(inputs), "undeclared inputs")
            inputs.update(overrides)
            require(len(encode(inputs)) <= 16384, "inputs exceed size limit")
            execution = uuid.uuid4().hex
            value = {
                "version": VERSION,
                "id": execution,
                "job": config["job"],
                "release": release,
                "image": identity,
                "definition": config,
                "definition_digest": digest(config),
                "compatibility": observed,
                "inputs": inputs,
                "created_at": time.time(),
                "expires_at": time.time() + config["retain_seconds"],
                "activations": [],
                "steps": [
                    {
                        "name": s["name"],
                        "id": execution + ":" + s["name"],
                        "state": "pending",
                        "outputs": {},
                        "attempts": [],
                    }
                    for s in config["steps"]
                ],
            }
        require(
            len(value["activations"]) < 100,
            "execution reached 100 activations; abandon it",
        )
        value["state"] = "running"
        value["invocation"] = invocation
        value["activations"].append(
            {
                "invocation": invocation,
                "operation": operation,
                "started_at": time.time(),
                "state": "running",
            }
        )
        store.write(value)
    return value["id"]


def validate_outputs(outputs, declared):
    require(
        isinstance(outputs, dict) and set(outputs) == set(declared),
        "outputs must contain exactly the declared keys",
    )
    require(len(encode(outputs)) <= 16384, "outputs exceed aggregate size limit")
    for value in outputs.values():
        require(
            isinstance(value, str)
            and "\x00" not in value
            and len(value.encode()) <= 4096,
            "outputs must be strings of at most 4096 bytes without NUL",
        )
    return outputs


def save_owned(store, value, invocation):
    with store.locked():
        current = store.read(value["id"])
        require(
            current["invocation"] == invocation and current["state"] == "running",
            "execution ownership changed",
        )
        store.write(value)


def update_run_status(store, value, invocation):
    path = store.root / "schedule" / (value["job"] + ".state")
    if not path.exists():
        return
    lines = path.read_text().splitlines()
    count = sum(
        a["invocation"] == invocation for s in value["steps"] for a in s["attempts"]
    )
    lines = [line for line in lines if not line.startswith("attempt=")]
    lines.append("attempt=" + str(count))
    atomic_bytes(path, ("\n".join(lines) + "\n").encode())


def execute(store, identity, invocation):
    value = store.read(identity)
    require(
        value["invocation"] == invocation and value["state"] == "running",
        "execution is not owned",
    )
    config = value["definition"]
    release_dir = store.root / "releases" / value["release"]
    deadline = time.monotonic() + config["timeout_seconds"]

    # systemd terminates the complete activation too. This handler ensures a
    # graceful timeout is checkpointed; SIGKILL/reboot is reconciled as interrupted.
    def interrupted(_signum, _frame):
        raise InterruptedError("activation interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    print("onebox: execution " + identity, flush=True)
    result = 1
    try:
        for index, definition in enumerate(config["steps"]):
            step = value["steps"][index]
            if step["state"] == "succeeded":
                continue
            backoff = definition["backoff"]
            for batch_attempt in range(definition["attempts"]):
                if time.monotonic() >= deadline:
                    raise TimeoutError("activation timeout")
                require(
                    not container_running(config["container"]),
                    "previous container is still running",
                )
                cleanup_container(config)
                step["state"] = "running"
                attempt = {
                    "id": uuid.uuid4().hex,
                    "number": len(step["attempts"]) + 1,
                    "invocation": invocation,
                    "state": "running",
                    "started_at": time.time(),
                }
                step["attempts"].append(attempt)
                save_owned(store, value, invocation)
                update_run_status(store, value, invocation)
                env = dict(value["inputs"])
                for key, ref in definition["inputs"].items():
                    source, output = ref.split(".")
                    env[key] = next(s for s in value["steps"] if s["name"] == source)[
                        "outputs"
                    ][output]
                env.update(
                    ONEBOX_EXECUTION_ID=identity,
                    ONEBOX_STEP_ID=step["id"],
                    ONEBOX_ATTEMPT_ID=attempt["id"],
                    ONEBOX_OUTPUT_FILE="/onebox-output/result.json",
                )
                # This directory is disposable, separate from the host-owned store.
                with tempfile.TemporaryDirectory(
                    prefix="output-", dir=store.path
                ) as output_dir:
                    os.chmod(output_dir, 0o777)
                    # Override the resolved image with its original content ID and
                    # disable pulling. A moved tag cannot change a resumed attempt.
                    override = Path(output_dir) / "compose.json"
                    override.write_bytes(
                        encode({"services": {config["job"]: {"image": value["image"]}}})
                    )
                    os.chmod(override, 0o600)
                    args = compose(config, release_dir) + [
                        "-f",
                        str(override),
                        "run",
                        "--rm",
                        "--no-deps",
                        "--pull",
                        "never",
                        "--name",
                        config["container"],
                        "--label",
                        "ob.execution.job=" + config["job"],
                        "--label",
                        "ob.execution.invocation=" + invocation,
                        "--volume",
                        output_dir + ":/onebox-output",
                    ]
                    for key, val in sorted(env.items()):
                        args += ["-e", key + "=" + val]
                    args += [config["job"]] + definition["command"]
                    print(
                        "onebox: step "
                        + step["name"]
                        + " attempt "
                        + str(attempt["number"]),
                        flush=True,
                    )
                    try:
                        proc = subprocess.run(
                            ["/usr/bin/docker"] + args,
                            check=False,
                            timeout=max(0.01, deadline - time.monotonic()),
                        )
                        attempt["exit_status"] = proc.returncode
                        outputs = {}
                        if proc.returncode == 0 and definition["outputs"]:
                            outputs = read_json(Path(output_dir) / "result.json", 16384)
                        if proc.returncode == 0:
                            step["outputs"] = validate_outputs(
                                outputs, definition["outputs"]
                            )
                            step["state"] = attempt["state"] = "succeeded"
                        else:
                            step["state"] = attempt["state"] = "failed"
                    except InterruptedError:
                        raise
                    except (ValueError, OSError) as error:
                        step["state"] = attempt["state"] = "failed"
                        attempt["reason"] = "output validation or execution failed"
                        print("onebox: " + str(error), file=sys.stderr)
                    finally:
                        cleanup_container(config, invocation)
                attempt["finished_at"] = time.time()
                save_owned(store, value, invocation)
                if step["state"] == "succeeded":
                    break
                if batch_attempt + 1 < definition["attempts"]:
                    if time.monotonic() + backoff >= deadline:
                        raise TimeoutError("activation timeout during backoff")
                    time.sleep(backoff)
                    backoff = min(backoff * 2, definition["max_backoff"])
            if step["state"] != "succeeded":
                value["state"] = "failed"
                break
        else:
            value["state"] = "succeeded"
            result = 0
    except (TimeoutError, subprocess.TimeoutExpired):
        value["state"] = "timeout"
    except InterruptedError:
        value["state"] = "interrupted"
    finally:
        for step in value["steps"]:
            if step["state"] == "running":
                step["state"] = "interrupted"
                step["attempts"][-1]["state"] = "interrupted"
        if value["state"] == "running":
            value["state"] = "interrupted"
        value["activations"][-1].update(state=value["state"], finished_at=time.time())
        save_owned(store, value, invocation)
    return result


def view(value, root):
    # A read derives interruption without rewriting evidence or requiring locks.
    if value["state"] == "running":
        config = value["definition"]
        probe = subprocess.run(
            [
                "systemctl",
                "show",
                config["unit"] + ".service",
                "--property=InvocationID",
                "--property=ActiveState",
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        require(probe.returncode == 0, "cannot inspect execution unit")
        fields = dict(
            line.split("=", 1) for line in probe.stdout.splitlines() if "=" in line
        )
        active = fields.get("ActiveState") in {"active", "activating", "deactivating"}
        if not (active and fields.get("InvocationID") == value["invocation"]):
            value["state"] = "interrupted"
            for step in value["steps"]:
                if step["state"] == "running":
                    step["state"] = "interrupted"
    value["expired"] = time.time() >= value["expires_at"]
    value["resumable"] = (
        value["state"] not in TERMINAL | {"running"} and not value["expired"]
    )
    value["current_release_matches"] = (root / "current").resolve().name == value[
        "release"
    ]
    return value


def abandon(store, identity):
    value = store.read(identity)
    path = store.root / "schedule" / (value["job"] + ".lock")
    # The Go caller holds the application lock; this mutex also excludes an
    # already running pinned job, which intentionally permits that app lock.
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError(
                "job is locked by an active operation; retry abandonment after it finishes"
            ) from None
        require(
            not container_running(value["definition"]["container"]),
            "job container is still running",
        )
        with store.locked():
            value = store.read(identity)
            require(
                value["state"] != "succeeded",
                "successful execution cannot be abandoned",
            )
            value["state"] = "abandoned"
            store.write(value)
    finally:
        os.close(fd)


def main(args):
    counts = {
        "prepare": 8,
        "run": 4,
        "inspect": 3,
        "list": 3,
        "pins": 2,
        "abandon": 3,
        "invalidate": 2,
    }
    require(
        bool(args) and args[0] in counts, "unknown or missing durable execution command"
    )
    require(len(args) == counts[args[0]], "invalid argument count for " + args[0])
    command, root = args[:2]
    store = Store(root)
    if command == "prepare":
        config = json.loads(base64.b64decode(args[2]))
        print(prepare(store, config, *args[3:7], json.loads(args[7])))
    elif command == "run":
        return execute(store, *args[2:4])
    elif command == "inspect":
        print(json.dumps(view(store.read(args[2]), store.root)))
    elif command == "list":
        values = store.records()
        values.sort(key=lambda v: v["created_at"], reverse=True)
        print(json.dumps([view(v, store.root) for v in values[: int(args[2])]]))
    elif command == "pins":
        # Running/interrupted work is conservatively protected even after its
        # expiry until explicitly abandoned. Expiry never proves a worker stopped.
        print(
            "\n".join(
                sorted(
                    {
                        v["release"]
                        for v in store.records()
                        if v["state"] not in TERMINAL
                        and (v["state"] == "running" or time.time() < v["expires_at"])
                    }
                )
            )
        )
    elif command == "abandon":
        abandon(store, args[2])
    elif command == "invalidate":
        with store.locked():
            atomic(store.path / ".data-generation", uuid.uuid4().hex)
    else:
        raise ValueError("unknown durable execution command")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except (ValueError, OSError, KeyError, TypeError, IndexError) as error:
        print("onebox: durable execution refused: " + str(error), file=sys.stderr)
        sys.exit(1)
