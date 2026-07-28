/*
 * pnp-mount: minimal mount(8) replacement -- the devenv image ships no
 * /bin/mount.  Only usable where the caller holds CAP_SYS_ADMIN over the
 * user namespace owning the mount namespace (i.e. inside userns-exec).
 *
 *   pnp-mount <type> <source> <target> [options]
 *   pnp-mount --bind <source> <target>
 *   pnp-mount --rprivate <target>
 */
#define _GNU_SOURCE
#include <stdio.h>
#include <string.h>
#include <errno.h>
#include <stdlib.h>
#include <sys/mount.h>

int main(int argc, char **argv)
{
    if (argc >= 2 && !strcmp(argv[1], "--bind")) {
        if (argc < 4) { fprintf(stderr, "usage: pnp-mount --bind src target\n"); return 64; }
        if (mount(argv[2], argv[3], NULL, MS_BIND | MS_REC, NULL) < 0) {
            fprintf(stderr, "pnp-mount: bind %s -> %s: %s\n", argv[2], argv[3], strerror(errno));
            return 1;
        }
        return 0;
    }
    if (argc >= 2 && !strcmp(argv[1], "--rprivate")) {
        if (argc < 3) { fprintf(stderr, "usage: pnp-mount --rprivate target\n"); return 64; }
        if (mount(NULL, argv[2], NULL, MS_REC | MS_PRIVATE, NULL) < 0) {
            fprintf(stderr, "pnp-mount: rprivate %s: %s\n", argv[2], strerror(errno));
            return 1;
        }
        return 0;
    }
    if (argc < 4) {
        fprintf(stderr, "usage: pnp-mount <type> <source> <target> [options]\n");
        return 64;
    }
    if (mount(argv[2], argv[3], argv[1], 0, argc > 4 ? argv[4] : NULL) < 0) {
        fprintf(stderr, "pnp-mount: mount -t %s %s %s: %s\n", argv[1], argv[2], argv[3], strerror(errno));
        return 1;
    }
    return 0;
}
