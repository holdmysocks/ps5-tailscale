/* In-process loader for a Go program built for the PS5 (GOOS=freebsd,
 * -buildmode=pie, internal linking).
 *
 * The SDK crt has already widened the address range that is allowed to issue
 * syscalls, so the Go runtime can use its own raw SYSCALL instructions. All
 * that is left is to map the image, relocate it, and enter it the way the
 * FreeBSD kernel would: %rdi pointing at argc/argv/envp/auxv. */

#include <elf.h>
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include <sys/mman.h>

#include <ps5/kernel.h>

#include "goload.h"
#include "report.h"

#define PS5_PAGE_SIZE 0x4000ul
#define PAGE_TRUNC(x) ((x) & ~(PS5_PAGE_SIZE - 1))
#define PAGE_ROUND(x) PAGE_TRUNC((x) + PS5_PAGE_SIZE - 1)

#define GO_STACK_SIZE (1ul << 20)

#ifndef AT_PAGESZ
#define AT_PAGESZ 6
#endif
#ifndef AT_NULL
#define AT_NULL 0
#endif

void goload_enter(uintptr_t entry, uintptr_t *args, void *stack_top) __attribute__((noreturn));

/* Switch to the new stack and jump to the Go entry point. Never returns. */
__asm__(".intel_syntax noprefix\n"
        ".text\n"
        ".global goload_enter\n"
        ".type goload_enter, @function\n"
        "goload_enter:\n"
        "  mov rax, rdi\n"
        "  mov rdi, rsi\n"
        "  mov rsp, rdx\n"
        "  xor ebp, ebp\n"
        "  jmp rax\n"
        ".att_syntax prefix\n");

#ifdef GOLOAD_DEBUG
#include <signal.h>
#include <stdlib.h>
#include <ucontext.h>

static uintptr_t dbg_base;
static uintptr_t dbg_vaddr;

/* Report faults that happen before the Go runtime installs its own signal
 * handlers. Only async-signal-safe calls are used. */
static void
dbg_fault(int sig, siginfo_t *info, void *uctx) {
  ucontext_t *uc = uctx;
  /* The PS5 kernel puts 0x30 extra bytes between uc_sigmask and uc_mcontext. */
  mcontext_t *mc = (mcontext_t *)((char *)&uc->uc_mcontext + 0x30);
  char buf[1024];
  uintptr_t rip = mc->mc_rip;
  int n = snprintf(buf, sizeof(buf),
                   "goload: fatal signal %d code=%d addr=%p rip=%#lx (image vaddr %#lx) rsp=%#lx\n"
                   "  rax=%#lx rbx=%#lx rcx=%#lx rdx=%#lx rsi=%#lx rdi=%#lx rbp=%#lx\n"
                   "  r8=%#lx r9=%#lx r10=%#lx r11=%#lx r12=%#lx r13=%#lx r14=%#lx r15=%#lx\n"
                   "  trapno=%ld err=%#lx flags=%#lx fsbase=%#lx getpid=%p\n",
                   sig, info->si_code, info->si_addr, (unsigned long)rip,
                   (unsigned long)(rip - dbg_base + dbg_vaddr), (unsigned long)mc->mc_rsp,
                   (unsigned long)mc->mc_rax, (unsigned long)mc->mc_rbx, (unsigned long)mc->mc_rcx,
                   (unsigned long)mc->mc_rdx, (unsigned long)mc->mc_rsi, (unsigned long)mc->mc_rdi,
                   (unsigned long)mc->mc_rbp, (unsigned long)mc->mc_r8, (unsigned long)mc->mc_r9,
                   (unsigned long)mc->mc_r10, (unsigned long)mc->mc_r11, (unsigned long)mc->mc_r12,
                   (unsigned long)mc->mc_r13, (unsigned long)mc->mc_r14, (unsigned long)mc->mc_r15,
                   (long)mc->mc_trapno, (unsigned long)mc->mc_err, (unsigned long)mc->mc_rflags,
                   (unsigned long)mc->mc_fsbase, (void *)getpid);
  write(2, buf, n);
  _exit(128 + sig);
}

static void
dbg_install(uintptr_t base, uintptr_t vaddr) {
  static const int sigs[] = {SIGSEGV, SIGBUS, SIGILL, SIGFPE, SIGSYS, SIGTRAP, SIGABRT};
  struct sigaction sa = {0};
  stack_t ss = {0};

  dbg_base = base;
  dbg_vaddr = vaddr;
  ss.ss_size = 64 * 1024;
  ss.ss_sp = malloc(ss.ss_size);
  sigaltstack(&ss, 0);
  sa.sa_sigaction = dbg_fault;
  sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
  sigfillset(&sa.sa_mask);
  for (size_t i = 0; i < sizeof(sigs) / sizeof(sigs[0]); i++) {
    sigaction(sigs[i], &sa, 0);
  }
}
#endif

static int
image_check(const uint8_t *image, size_t size) {
  const Elf64_Ehdr *ehdr = (const Elf64_Ehdr *)image;

  if (size < sizeof(*ehdr) || memcmp(ehdr->e_ident, ELFMAG, SELFMAG)) {
    return -1;
  }
  if (ehdr->e_type != ET_DYN || ehdr->e_machine != EM_X86_64) {
    return -1;
  }
  if (ehdr->e_phoff + (size_t)ehdr->e_phnum * sizeof(Elf64_Phdr) > size) {
    return -1;
  }
  return 0;
}

int
goload_run(const uint8_t *image, size_t size, char *const argv[], char *const envp[]) {
  const Elf64_Ehdr *ehdr = (const Elf64_Ehdr *)image;
  const Elf64_Phdr *phdr = (const Elf64_Phdr *)(image + ehdr->e_phoff);
  const Elf64_Dyn *dyn = 0;
  const Elf64_Rela *rela = 0;
  size_t relasz = 0;
  uintptr_t min_vaddr = UINTPTR_MAX;
  uintptr_t max_vaddr = 0;
  uintptr_t bias;
  uint8_t *base;
  uint8_t *stack;
  uintptr_t *sp;
  int argc = 0;
  int envc = 0;

  if (image_check(image, size)) {
    report_fail("goload: the embedded program is not a relocatable x86-64 ELF image");
    return -1;
  }

  for (int i = 0; i < ehdr->e_phnum; i++) {
    if (phdr[i].p_type != PT_LOAD || !phdr[i].p_memsz) {
      continue;
    }
    if (phdr[i].p_offset + phdr[i].p_filesz > size) {
      report_fail("goload: the embedded program is truncated");
      return -1;
    }
    if (phdr[i].p_vaddr < min_vaddr) {
      min_vaddr = phdr[i].p_vaddr;
    }
    if (phdr[i].p_vaddr + phdr[i].p_memsz > max_vaddr) {
      max_vaddr = phdr[i].p_vaddr + phdr[i].p_memsz;
    }
  }
  if (min_vaddr >= max_vaddr) {
    report_fail("goload: the embedded program has no loadable segments");
    return -1;
  }
  min_vaddr = PAGE_TRUNC(min_vaddr);
  max_vaddr = PAGE_ROUND(max_vaddr);

  base = mmap(0, max_vaddr - min_vaddr, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (base == MAP_FAILED) {
    report_fail("goload: no memory for the program (mmap: %s)", strerror(errno));
    return -1;
  }
  bias = (uintptr_t)base - min_vaddr;

  for (int i = 0; i < ehdr->e_phnum; i++) {
    if (phdr[i].p_type == PT_LOAD && phdr[i].p_filesz) {
      memcpy((void *)(bias + phdr[i].p_vaddr), image + phdr[i].p_offset, phdr[i].p_filesz);
    } else if (phdr[i].p_type == PT_DYNAMIC) {
      dyn = (const Elf64_Dyn *)(bias + phdr[i].p_vaddr);
    }
  }

  /* The Go linker only emits R_X86_64_RELATIVE for an internally linked PIE. */
  for (; dyn && dyn->d_tag != DT_NULL; dyn++) {
    if (dyn->d_tag == DT_RELA) {
      rela = (const Elf64_Rela *)(bias + dyn->d_un.d_ptr);
    } else if (dyn->d_tag == DT_RELASZ) {
      relasz = dyn->d_un.d_val;
    }
  }
  for (size_t i = 0; rela && i < relasz / sizeof(*rela); i++) {
    if (ELF64_R_TYPE(rela[i].r_info) != R_X86_64_RELATIVE) {
      report_fail("goload: unsupported relocation type %u", (unsigned)ELF64_R_TYPE(rela[i].r_info));
      return -1;
    }
    *(uintptr_t *)(bias + rela[i].r_offset) = bias + rela[i].r_addend;
  }

  /* Segments are 4 KiB aligned but the PS5 uses 16 KiB pages, so protection
   * is applied to whole pages: executable ones become R+X, the rest stays
   * R+W. A page shared by text and read-only data ends up executable, which
   * is harmless. */
  for (int i = 0; i < ehdr->e_phnum; i++) {
    if (phdr[i].p_type != PT_LOAD || !(phdr[i].p_flags & PF_X)) {
      continue;
    }
    uintptr_t start = PAGE_TRUNC(bias + phdr[i].p_vaddr);
    uintptr_t end = PAGE_ROUND(bias + phdr[i].p_vaddr + phdr[i].p_memsz);
    /* kernel_mprotect rewrites the protection of the whole kernel map entry
     * that contains the address, so first let a regular mprotect split the
     * text range off into an entry of its own. */
    if (mprotect((void *)start, end - start, PROT_READ)) {
      report_fail("goload: mprotect: %s", strerror(errno));
      return -1;
    }
    if (kernel_mprotect(-1, start, end - start, PROT_READ | PROT_EXEC)) {
      report_fail("goload: could not make the program executable (kernel_mprotect failed); "
                  "this firmware or jailbreak may not allow it");
      return -1;
    }
  }

  stack = mmap(0, GO_STACK_SIZE, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (stack == MAP_FAILED) {
    report_fail("goload: no memory for the stack (mmap: %s)", strerror(errno));
    return -1;
  }

  while (argv && argv[argc]) {
    argc++;
  }
  while (envp && envp[envc]) {
    envc++;
  }

  /* argc, argv[], NULL, envp[], NULL, auxv pairs, AT_NULL */
  sp = (uintptr_t *)(stack + GO_STACK_SIZE);
  sp -= 1 + (argc + 1) + (envc + 1) + 4;
  sp = (uintptr_t *)((uintptr_t)sp & ~0xful);

  uintptr_t *p = sp;
  *p++ = (uintptr_t)argc;
  for (int i = 0; i < argc; i++) {
    *p++ = (uintptr_t)argv[i];
  }
  *p++ = 0;
  for (int i = 0; i < envc; i++) {
    *p++ = (uintptr_t)envp[i];
  }
  *p++ = 0;
  *p++ = AT_PAGESZ;
  *p++ = PS5_PAGE_SIZE;
  *p++ = AT_NULL;
  *p++ = 0;

#ifdef GOLOAD_DEBUG
  fprintf(stderr, "goload: image %p..%p entry %#lx stack %p..%p argc=%d\n", base,
          base + (max_vaddr - min_vaddr), (unsigned long)(bias + ehdr->e_entry), stack, stack + GO_STACK_SIZE, argc);
  dbg_install((uintptr_t)base, min_vaddr);
#else
  /* From here on nothing is printed by the launcher. If the Go runtime dies
   * before the daemon has opened its own log, its message lands in the
   * launcher log instead of being lost with the sender's connection. */
  report_log("launcher: starting the Go program");
  fflush(stderr);
  report_capture_stderr();
#endif

  fflush(stdout);
  fflush(stderr);

  /* The embedded image has been copied and is not read again. Let the kernel
   * reclaim its pages rather than keep two copies of the program in memory. */
  uintptr_t entry = bias + ehdr->e_entry;
  uintptr_t free_start = PAGE_ROUND((uintptr_t)image);
  uintptr_t free_end = PAGE_TRUNC((uintptr_t)image + size);
  if (free_end > free_start) {
    madvise((void *)free_start, free_end - free_start, MADV_FREE);
  }

  goload_enter(entry, sp, sp);
}
