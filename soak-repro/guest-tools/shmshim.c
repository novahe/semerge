/* shmshim.c — LD_PRELOAD 垫片：sandbox 内核没有 SysV IPC（shmget ENOSYS），
 * 用 MAP_SHARED|MAP_ANONYMOUS 实现最小 shmget/shmat/shmdt/shmctl，够 fio 使用。
 * 仅支持 IPC_PRIVATE 场景（fio 就是这么用的：分配→fork 继承映射→attach 同地址）。
 */
#define _GNU_SOURCE
#include <sys/mman.h>
#include <sys/ipc.h>
#include <sys/shm.h>
#include <errno.h>
#include <stddef.h>

#define MAX_SEGS 64
struct seg {
    int id;
    void *addr;
    size_t size;
    int nattch;
    int marked_remove;
};
static struct seg segs[MAX_SEGS];
static int next_id = 1;

static struct seg *find_free(void) {
    for (int i = 0; i < MAX_SEGS; i++)
        if (segs[i].id == 0)
            return &segs[i];
    return NULL;
}

static struct seg *find(int shmid) {
    for (int i = 0; i < MAX_SEGS; i++)
        if (segs[i].id == shmid)
            return &segs[i];
    return NULL;
}

int shmget(key_t key, size_t size, int shmflg) {
    (void)key; (void)shmflg;   /* 只支持 IPC_PRIVATE 语义 */
    struct seg *s = find_free();
    if (!s) { errno = ENOSPC; return -1; }
    void *p = mmap(NULL, size, PROT_READ | PROT_WRITE,
                   MAP_SHARED | MAP_ANONYMOUS, -1, 0);
    if (p == MAP_FAILED) return -1;
    s->id = next_id++;
    s->addr = p;
    s->size = size;
    s->nattch = 0;
    s->marked_remove = 0;
    return s->id;
}

void *shmat(int shmid, const void *shmaddr, int shmflg) {
    struct seg *s = find(shmid);
    if (!s) { errno = EINVAL; return (void *)-1; }
    if (shmaddr != NULL && !(shmflg & SHM_REMAP)) {
        errno = EINVAL; return (void *)-1;   /* fio 传 NULL */
    }
    s->nattch++;
    return s->addr;
}

int shmdt(const void *shmaddr) {
    for (int i = 0; i < MAX_SEGS; i++) {
        if (segs[i].id && segs[i].addr == shmaddr) {
            if (--segs[i].nattch <= 0 && segs[i].marked_remove) {
                munmap(segs[i].addr, segs[i].size);
                segs[i].id = 0;
            }
            return 0;
        }
    }
    errno = EINVAL;
    return -1;
}

int shmctl(int shmid, int cmd, struct shmid_ds *buf) {
    struct seg *s = find(shmid);
    if (!s) { errno = EINVAL; return -1; }
    switch (cmd) {
    case IPC_RMID:
        s->marked_remove = 1;              /* 映射留给 attach 方，进程退出即回收 */
        return 0;
    case IPC_STAT:
        if (buf) {
            __builtin_memset(buf, 0, sizeof(*buf));
            buf->shm_segsz = s->size;
            buf->shm_nattch = s->nattch;
        }
        return 0;
    default:
        errno = EINVAL;
        return -1;
    }
}
