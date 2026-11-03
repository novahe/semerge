// SPDX-License-Identifier: Apache-2.0
// Mechanism probe only: WP registration without applying write protection.
// This is not a VM benchmark or a replacement for the restore handler.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/userfaultfd.h>
#include <poll.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <unistd.h>

static int ufd;
static atomic_int stop_reader, faults, removes, unmaps, reader_error;
static void fail(const char *message) { perror(message); exit(1); }
static void require(int ok, const char *message) {
    if (!ok) { fprintf(stderr, "%s\n", message); exit(1); }
}
static void *read_events(void *unused) {
    (void)unused;
    while (!atomic_load(&stop_reader)) {
        struct pollfd pfd = { .fd = ufd, .events = POLLIN };
        int ready = poll(&pfd, 1, 50);
        if (ready < 0 && errno == EINTR) continue;
        if (ready < 0 || (pfd.revents & (POLLERR | POLLHUP | POLLNVAL))) {
            atomic_store(&reader_error, 1); return NULL;
        }
        if (!ready) continue;
        struct uffd_msg message;
        ssize_t n = read(ufd, &message, sizeof(message));
        if (n < 0 && (errno == EAGAIN || errno == EINTR)) continue;
        if (n != sizeof(message)) { atomic_store(&reader_error, 1); return NULL; }
        switch (message.event) {
        case UFFD_EVENT_REMOVE: atomic_fetch_add(&removes, 1); break;
        case UFFD_EVENT_UNMAP: atomic_fetch_add(&unmaps, 1); break;
        case UFFD_EVENT_PAGEFAULT:
            atomic_fetch_add(&faults, 1);
            // Never supply guessed contents to an unexpected fault.
            fprintf(stderr, "unexpected PAGEFAULT flags=%llu\n",
                    (unsigned long long)message.arg.pagefault.flags);
            exit(1);
        default: atomic_store(&reader_error, 1); return NULL;
        }
    }
    return NULL;
}
int main(void) {
    alarm(10);
    size_t page = (size_t)sysconf(_SC_PAGESIZE), length = 2 * page;
    int fd = memfd_create("cold-zero-wp-probe", MFD_CLOEXEC);
    if (fd < 0 || ftruncate(fd, (off_t)length)) fail("memfd");
    unsigned char *cpu = mmap(NULL, length, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    unsigned char *backend = mmap(NULL, length, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (cpu == MAP_FAILED || backend == MAP_FAILED) fail("mmap");
    memset(backend + page, 0x77, page); // Existing nonzero page must survive.
    int dev = open("/dev/userfaultfd", O_RDWR | O_CLOEXEC);
    ufd = dev < 0 ? -1 : ioctl(dev, USERFAULTFD_IOC_NEW, O_CLOEXEC | O_NONBLOCK);
    if (dev >= 0) close(dev);
    if (ufd < 0) ufd = (int)syscall(SYS_userfaultfd, O_CLOEXEC | O_NONBLOCK);
    if (ufd < 0) fail("create userfaultfd");
    uint64_t required = UFFD_FEATURE_EVENT_REMOVE | UFFD_FEATURE_EVENT_UNMAP |
                        UFFD_FEATURE_WP_HUGETLBFS_SHMEM;
    struct uffdio_api api = { .api = UFFD_API, .features = required };
    if (ioctl(ufd, UFFDIO_API, &api)) fail("UFFDIO_API");
    require((api.features & required) == required, "required kernel features missing");
    struct uffdio_register reg = {
        .range = { .start = (uint64_t)(uintptr_t)cpu, .len = length },
        .mode = UFFDIO_REGISTER_MODE_WP,
    };
    if (ioctl(ufd, UFFDIO_REGISTER, &reg)) fail("register WP-only");
    // Deliberately do not issue UFFDIO_WRITEPROTECT.
    pthread_t reader;
    if (pthread_create(&reader, NULL, read_events, NULL)) fail("pthread_create");
    for (size_t i = 0; i < page; ++i) {
        require(((volatile unsigned char *)cpu)[i] == 0, "fresh page was not zero");
        require(((volatile unsigned char *)cpu)[page + i] == 0x77, "existing data changed");
    }
    memset(cpu, 0x5a, page);
    require(backend[0] == 0x5a, "native write not shared with backend");
    if (madvise(cpu, page, MADV_DONTNEED)) fail("MADV_DONTNEED");
    if (fallocate(fd, FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE, 0, (off_t)page)) fail("punch");
    for (size_t i = 0; i < page; ++i) {
        require(((volatile unsigned char *)cpu)[i] == 0, "punched page was not zero");
        require(((volatile unsigned char *)cpu)[page + i] == 0x77, "neighbor data changed");
    }
    backend[0] = 0x42;
    require(((volatile unsigned char *)cpu)[0] == 0x42, "backend write was not preserved");
    if (munmap(cpu, length)) fail("munmap registered alias");
    for (int i = 0; i < 100 && atomic_load(&unmaps) == 0; ++i) usleep(1000);
    atomic_store(&stop_reader, 1);
    pthread_join(reader, NULL);
    require(!atomic_load(&reader_error), "reader failed");
    require(atomic_load(&faults) == 0 && atomic_load(&removes) >= 1 && atomic_load(&unmaps) >= 1,
            "unexpected event counts");
    printf("{\"ok\":true,\"page_size\":%zu,\"pagefault_events\":%d,\"remove_events\":%d,"
           "\"unmap_events\":%d,\"fresh_zero\":true,\"existing_data_preserved\":true,"
           "\"punch_refault_zero\":true,\"backend_write_preserved\":true}\n",
           page, atomic_load(&faults), atomic_load(&removes), atomic_load(&unmaps));
    munmap(backend, length); close(ufd); close(fd);
    return 0;
}
