#!/usr/bin/env python3
"""Run podman Quadlet units without systemd.

systemd cannot run in the nested environment D-013 selected -- the surrounding
container's own cgroup is a *threaded* cgroup2 domain, so even PID 1 cannot
`mkdir /init.scope` -- but the Quadlet *generator* is a plain binary that needs
nothing but a unit directory. This drives it directly:

    quadlet-supervisor.py print   <unit-dir> [unit ...]
    quadlet-supervisor.py start   <unit-dir> [unit ...]
    quadlet-supervisor.py stop    <unit-dir> [unit ...]
    quadlet-supervisor.py status
    quadlet-supervisor.py is-active <unit>

so the very same .container/.volume/.network/.pod files the deployment ships
are what gets exercised, minus the parts of systemd that are not being tested.

What is substituted, and why it is still an honest test:

  * Specifier expansion.  The deployment's units are *user* units (D-009) that
    name %h/.config/hirame/...; quadlet passes %h through into the generated
    ExecStart for systemd to expand at load time, and there is no systemd here,
    so expand() does it against the same $HOME e2e.sh installed those files
    under. An unexpanded specifier would reach podman as a relative path that
    it creates as an empty directory -- a silent failure, not a loud one.
  * Requires= / After=.  Reimplemented here, because D-005's guarantee is
    precisely that a failed migration stops its dependents. A unit whose
    Requires= names a unit that failed or was skipped is recorded `skipped` and
    its ExecStart never runs -- which is what `systemctl` does for a
    Requires=+After= pair, and what scenario S2 asserts.
  * Notify=healthy.  There is no NOTIFY_SOCKET, so the generated
    --sdnotify=healthy is rewritten to =ignore and readiness is instead
    established by running the unit's own HealthCmd through
    `podman healthcheck run` until it passes. The probe is the deployed one;
    only the thing that waits for it changed.

What is NOT substituted: Restart=, RestartSec=, and socket activation. Nothing
here restarts a crashed container, so a scenario that needs a restart has to
ask for one.
"""

import json
import os
import re
import shlex
import subprocess
import sys
import time

PODMAN_DIST = os.environ.get("PODMAN_DIST", "/root/.local/share/podman-dist/current")
QUADLET = os.environ.get(
    "PNP_QUADLET", PODMAN_DIST + "/usr/local/libexec/podman/quadlet"
)
PODMAN = os.environ.get("PNP_PODMAN", PODMAN_DIST + "/usr/local/bin/podman")
RUNTIME_DIR = os.environ.get("XDG_RUNTIME_DIR", "/run")
STATE_FILE = os.environ.get("PNP_SUPERVISOR_STATE", "/run/pnp/quadlet-supervisor.json")

# Fallback for TimeoutStartSec=. Every long-starting unit in deploy/quadlet/
# sets its own; this only covers a unit that does not.
DEFAULT_TIMEOUT = 90


# --------------------------------------------------------------------------
# generator
# --------------------------------------------------------------------------


def generate(unit_dir):
    """Return {service_name: {key: [values]}} from `quadlet -user -dryrun`.

    QUADLET_UNIT_DIRS must be absolute: the generator resolves drop-in
    directories against it and silently finds none for a relative path.

    -user because the deployed units are user units (D-009). It is very nearly
    inert here: diffing the two modes over this unit set shows the flag changes
    only the synthesised network ordering -- Wants=/After=network-online.target
    against podman-user-wait-network-online.service -- and neither name is a
    unit this supervisor generates, so `order()` never sees either. It is
    passed anyway so the generator is invoked the way the deployment invokes
    it, rather than in a mode nothing uses.
    """
    env = dict(os.environ, QUADLET_UNIT_DIRS=os.path.abspath(unit_dir))
    proc = subprocess.run(
        [QUADLET, "-user", "-dryrun", "-no-kmsg-log"],
        env=env,
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode:
        sys.exit("quadlet generator failed:\n" + proc.stderr)
    units, name, body = {}, None, []
    for line in proc.stdout.splitlines():
        m = re.match(r"^---(.+)---$", line)
        if m:
            if name:
                units[name] = parse(name, body)
            name, body = m.group(1), []
        elif name:
            body.append(line)
    if name:
        units[name] = parse(name, body)
    return units


def parse(name, lines):
    unit = {}
    for line in lines:
        if "=" not in line or line.startswith("["):
            continue
        k, v = line.split("=", 1)
        unit.setdefault(k.strip(), []).append(expand(v, name))
    return unit


def expand(value, unit_name):
    """The systemd specifiers quadlet emits, plus the C-escapes it applies.

    %h matters most, and is the reason this function exists: the deployment's
    units are *user* units (D-009) that name their environment files and
    postgresql.conf as %h/.config/hirame/..., and quadlet deliberately passes
    %h through into the generated ExecStart for systemd to expand at load time.
    There is no systemd here to do that, so an unexpanded one would reach
    podman as a literal relative path that it then creates as an empty
    directory -- a silent failure, not a loud one.

    %t is load-bearing too (quadlet emits RequiresMountsFor=%t/containers
    itself). The rest are kept for the same silent-failure reason.
    """
    stem = unit_name.rsplit(".", 1)[0]
    home = os.path.expanduser("~")
    for spec, repl in (
        ("%n", unit_name),
        ("%N", stem),
        ("%t", RUNTIME_DIR),
        ("%h", home),
        ("%E", os.path.join(home, ".config")),
        ("%C", os.path.join(home, ".cache")),
        ("%S", os.path.join(home, ".local/share")),
        ("%%", "%"),
    ):
        value = value.replace(spec, repl)
    return re.sub(r"\\x([0-9a-fA-F]{2})", lambda m: chr(int(m.group(1), 16)), value)


def argv_of(cmdline):
    """Strip systemd's Exec* prefixes (-, @, +, !) and split."""
    cmdline = cmdline.lstrip("-@+!:")
    argv = shlex.split(cmdline)
    if argv and os.path.basename(argv[0]) == "podman":
        argv[0] = PODMAN
    out = []
    for a in argv:
        # No systemd means no NOTIFY_SOCKET, and podman refuses to start a
        # container it cannot notify through; readiness is re-established
        # below by running the unit's own HealthCmd.
        if a.startswith("--sdnotify="):
            a = "--sdnotify=ignore"
        # --cgroups=split needs a systemd scope to split from.
        if a == "--cgroups=split":
            a = "--cgroups=disabled"
        out.append(a)
    return out


# --------------------------------------------------------------------------
# unit properties
# --------------------------------------------------------------------------


def requires_of(unit):
    """Requires= of the *generated* unit.

    Quadlet synthesizes these from Pod=, Network= and Volume= as well as
    copying the source unit's own, so this is the full dependency set and the
    source files do not have to be re-read.
    """
    out = []
    for key in ("Requires", "BindsTo"):
        for line in unit.get(key, []):
            out.extend(line.split())
    return out


def after_of(unit):
    out = []
    for line in unit.get("After", []):
        out.extend(line.split())
    return out


def is_oneshot(unit):
    return "oneshot" in unit.get("Type", [])


def timeout_of(unit):
    for line in unit.get("TimeoutStartSec", []):
        try:
            return int(line.rstrip("s"))
        except ValueError:
            pass
    return DEFAULT_TIMEOUT


def container_name(argv):
    """The --name podman was given, which is what healthchecks address."""
    for i, a in enumerate(argv):
        if a == "--name" and i + 1 < len(argv):
            return argv[i + 1]
        if a.startswith("--name="):
            return a.split("=", 1)[1]
    return None


def health_cmd_present(argv):
    return any(a == "--health-cmd" or a.startswith("--health-cmd=") for a in argv)


# --------------------------------------------------------------------------
# state, shared across invocations
# --------------------------------------------------------------------------


def load_state():
    try:
        with open(STATE_FILE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(state):
    os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
    with open(STATE_FILE, "w") as f:
        json.dump(state, f, indent=1, sort_keys=True)


# --------------------------------------------------------------------------
# execution
# --------------------------------------------------------------------------


def unit_env(unit):
    env = dict(os.environ)
    for kv in unit.get("Environment", []):
        for pair in shlex.split(kv):
            if "=" in pair:
                k, v = pair.split("=", 1)
                env[k] = v
    return env


def run_key(unit, key, check=True):
    """Run every Exec* line of one key. Returns the first nonzero rc."""
    rc = 0
    for cmdline in unit.get(key, []):
        ignore = cmdline.lstrip().startswith("-")
        argv = argv_of(cmdline)
        print("+ " + " ".join(shlex.quote(a) for a in argv), file=sys.stderr)
        p = subprocess.run(argv, env=unit_env(unit), check=False)
        if p.returncode and not ignore:
            if check:
                return p.returncode
            rc = rc or p.returncode
    return rc


def await_ready(name, timeout):
    """Poll the container's own HealthCmd, the substitute for Notify=healthy.

    A container that exited is reported straight away rather than waited on:
    a crash loop would otherwise only surface as a timeout.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status = subprocess.run(
            [PODMAN, "inspect", "--format", "{{.State.Status}}", name],
            capture_output=True,
            text=True,
            check=False,
        ).stdout.strip()
        if status in ("exited", "died"):
            return False, f"container {name} exited while starting"
        probe = subprocess.run(
            [PODMAN, "healthcheck", "run", name], capture_output=True, check=False
        )
        if probe.returncode == 0:
            return True, ""
        time.sleep(2)
    return False, f"healthcheck of {name} did not pass within {timeout}s"


def start_unit(name, unit, state):
    blockers = [
        dep
        for dep in requires_of(unit)
        if state.get(dep, {}).get("state") in ("failed", "skipped")
    ]
    if blockers:
        # The D-005 guarantee: Requires= plus After= on a unit that failed to
        # activate means this one is not started at all.
        state[name] = {"state": "skipped", "detail": "requires " + ", ".join(blockers)}
        print(f"== SKIP {name} (requires {', '.join(blockers)})", file=sys.stderr)
        return

    print(f"== starting {name}", file=sys.stderr)
    rc = run_key(unit, "ExecStartPre")
    if rc:
        state[name] = {"state": "failed", "detail": f"ExecStartPre rc={rc}"}
        return
    rc = run_key(unit, "ExecStart")
    if rc:
        state[name] = {"state": "failed", "detail": f"ExecStart rc={rc}"}
        return

    argv = argv_of(unit.get("ExecStart", [""])[0])
    ctr = container_name(argv)
    if not is_oneshot(unit) and ctr and health_cmd_present(argv):
        ok, detail = await_ready(ctr, timeout_of(unit))
        if not ok:
            state[name] = {"state": "failed", "detail": detail}
            print(f"== FAILED {name}: {detail}", file=sys.stderr)
            return
        print(f"== ready {name}", file=sys.stderr)
    state[name] = {"state": "started", "detail": ""}


# --------------------------------------------------------------------------
# ordering
# --------------------------------------------------------------------------


def order(units, wanted):
    """Honour After= among the generated units; alphabetical otherwise."""
    names = [n for n in sorted(units) if not wanted or n in wanted]
    done, out = set(), []

    def visit(n, seen):
        if n in done or n in seen:
            return
        seen.add(n)
        for dep in after_of(units[n]):
            if dep in units and dep in names:
                visit(dep, seen)
        done.add(n)
        out.append(n)

    for n in names:
        visit(n, set())
    return out


# --------------------------------------------------------------------------
# commands
# --------------------------------------------------------------------------


def cmd_print(units, wanted):
    for n in order(units, wanted):
        print(f"=== {n} ===")
        for k, vs in units[n].items():
            for v in vs:
                print(f"{k}={v}")


def cmd_start(units, wanted):
    state = load_state()
    try:
        for n in order(units, wanted):
            start_unit(n, units[n], state)
    finally:
        save_state(state)
    return 1 if any(v["state"] == "failed" for v in state.values()) else 0


def cmd_stop(units, wanted):
    state = load_state()
    for n in reversed(order(units, wanted)):
        print(f"== stopping {n}", file=sys.stderr)
        run_key(units[n], "ExecStop", check=False)
        run_key(units[n], "ExecStopPost", check=False)
        state.pop(n, None)
    save_state(state)
    return 0


def cmd_status():
    state = load_state()
    width = max([len(n) for n in state] + [4])
    print(f"{'UNIT':<{width}}  {'STATE':<8}  DETAIL")
    for n in sorted(state):
        print(f"{n:<{width}}  {state[n]['state']:<8}  {state[n]['detail']}")
    return 0


def main():
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    cmd = sys.argv[1]
    if cmd == "status":
        return cmd_status()
    if cmd == "is-active":
        if len(sys.argv) < 3:
            sys.exit("usage: quadlet-supervisor.py is-active <unit>")
        return 0 if load_state().get(sys.argv[2], {}).get("state") == "started" else 1
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    unit_dir, wanted = sys.argv[2], set(sys.argv[3:])
    units = generate(unit_dir)
    unknown = wanted - set(units)
    if unknown:
        sys.exit("no such generated unit: " + ", ".join(sorted(unknown)))
    if cmd == "print":
        return cmd_print(units, wanted) or 0
    if cmd == "start":
        return cmd_start(units, wanted)
    if cmd == "stop":
        return cmd_stop(units, wanted)
    sys.exit(f"unknown command {cmd!r}")


if __name__ == "__main__":
    sys.exit(main())
