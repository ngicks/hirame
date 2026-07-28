/*
 * userns-exec: create a user namespace with a full 0..65536 id mapping and
 * exec a command inside it.
 *
 * The devenv container itself lives in a user namespace whose map is
 *     0     1000      1
 *     1   100000  65536
 * so ids 0..65536 exist for us.  A child user namespace can therefore map
 * "0 0 65537" identity-wise, which (a) gives the child full capabilities
 * (CAP_SYS_ADMIN, CAP_SETUID/SETGID, CAP_NET_ADMIN, CAP_MKNOD ...) inside
 * that namespace and (b) makes gid 5 / uid 999 etc. mappable, which is what
 * crun's devpts mount (gid=5) and multi-uid images (postgres) need.
 *
 * The maps are written by the PARENT (which holds CAP_SETUID/CAP_SETGID in
 * the namespace that owns the new one), so no newuidmap/newgidmap setuid
 * helper is required -- there is none in this image.
 */
#define _GNU_SOURCE
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/wait.h>
#include <sys/types.h>
#include <sys/mount.h>

static void die(const char *m) { fprintf(stderr, "userns-exec: %s: %s\n", m, strerror(errno)); exit(70); }

static int write_file(const char *path, const char *data)
{
    int fd = open(path, O_WRONLY);
    if (fd < 0) return -1;
    ssize_t n = write(fd, data, strlen(data));
    int e = errno;
    close(fd);
    if (n != (ssize_t)strlen(data)) { errno = e; return -1; }
    return 0;
}

int main(int argc, char **argv)
{
    const char *uidmap = getenv("PNP_UIDMAP");
    const char *gidmap = getenv("PNP_GIDMAP");
    const char *setgroups = getenv("PNP_SETGROUPS"); /* "allow" (default) or "deny" */
    int flags = CLONE_NEWUSER;
    int i = 1;

    if (!uidmap) uidmap = "0 0 65537\n";
    if (!gidmap) gidmap = "0 0 65537\n";
    if (!setgroups) setgroups = "allow";

    for (; i < argc; i++) {
        if (!strcmp(argv[i], "-m")) flags |= CLONE_NEWNS;
        else if (!strcmp(argv[i], "-n")) flags |= CLONE_NEWNET;
        else if (!strcmp(argv[i], "-p")) flags |= CLONE_NEWPID;
        else if (!strcmp(argv[i], "-i")) flags |= CLONE_NEWIPC;
        else if (!strcmp(argv[i], "-u")) flags |= CLONE_NEWUTS;
        else if (!strcmp(argv[i], "-C")) flags |= CLONE_NEWCGROUP;
        else if (!strcmp(argv[i], "--")) { i++; break; }
        else break;
    }
    if (i >= argc) {
        fprintf(stderr, "usage: userns-exec [-m][-n][-p][-i][-u][-C] [--] cmd [args...]\n");
        return 64;
    }

    int p2c[2], c2p[2];
    if (pipe(p2c) || pipe(c2p)) die("pipe");

    pid_t pid = fork();
    if (pid < 0) die("fork");

    if (pid == 0) {
        char c;
        close(p2c[1]); close(c2p[0]);
        if (unshare(flags) < 0) die("unshare");
        if (write(c2p[1], "x", 1) != 1) die("write c2p");
        if (read(p2c[0], &c, 1) != 1) { fprintf(stderr, "userns-exec: parent failed to set up maps\n"); _exit(71); }
        if (flags & CLONE_NEWNS) {
            /* Detach propagation so mounts made in here never leak back into
               the devenv (and so cgroup2 can be bind-mounted over the locked
               read-only /sys/fs/cgroup). */
            if (mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) < 0)
                fprintf(stderr, "userns-exec: warn: make-rprivate /: %s\n", strerror(errno));
        }
        if (setgid(0) < 0) die("setgid");
        if (setuid(0) < 0) die("setuid");
        execvp(argv[i], &argv[i]);
        die("execvp");
    }

    char c;
    char path[128];
    close(p2c[0]); close(c2p[1]);
    if (read(c2p[0], &c, 1) != 1) { fprintf(stderr, "userns-exec: child died before unshare\n"); return 71; }

    snprintf(path, sizeof(path), "/proc/%d/setgroups", (int)pid);
    if (write_file(path, setgroups) < 0)
        fprintf(stderr, "userns-exec: warn: setgroups=%s: %s\n", setgroups, strerror(errno));

    snprintf(path, sizeof(path), "/proc/%d/uid_map", (int)pid);
    if (write_file(path, uidmap) < 0) { fprintf(stderr, "userns-exec: uid_map: %s\n", strerror(errno)); return 72; }

    snprintf(path, sizeof(path), "/proc/%d/gid_map", (int)pid);
    if (write_file(path, gidmap) < 0) { fprintf(stderr, "userns-exec: gid_map: %s\n", strerror(errno)); return 72; }

    if (write(p2c[1], "x", 1) != 1) die("write p2c");

    int st = 0;
    if (waitpid(pid, &st, 0) < 0) die("waitpid");
    if (WIFEXITED(st)) return WEXITSTATUS(st);
    if (WIFSIGNALED(st)) return 128 + WTERMSIG(st);
    return 1;
}
