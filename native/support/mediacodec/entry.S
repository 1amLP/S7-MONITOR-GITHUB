/* Minimal dynamic Bionic startup. No C++ constructors/destructors in this worker.
 * __libc_init's four-argument ABI is present in the pinned original libc.so. */
.text
.global _start
.type _start,%function
_start:
 mov x0, sp
 mov x1, xzr
 adrp x2, s7_codec_main
 add x2, x2, :lo12:s7_codec_main
 adrp x3, s7_empty_structors
 add x3, x3, :lo12:s7_empty_structors
 b __libc_init
.size _start, .-_start
.section .rodata
.balign 8
s7_empty_structors:
 .quad 0,0,0
.section .note.GNU-stack,"",%progbits
