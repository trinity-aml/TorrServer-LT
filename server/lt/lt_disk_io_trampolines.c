/*
 * C trampolines that glue Go-exported storage callbacks
 * (//export tsl_storage_*_go) into the C++ disk_io's callback struct.
 *
 * Compiled as plain C; lives in this package directory so cgo picks it up
 * automatically alongside the .cpp implementation files.
 */

#include "lt_disk_io.h"

/* Forward declarations of //export'ed Go functions (defined in lt.go). */
extern void tsl_storage_open_go   (long long storage_id, const unsigned char* info_hash_20, int num_pieces, long long piece_length);
extern void tsl_storage_close_go  (long long storage_id);
extern void tsl_storage_deleted_go(long long storage_id);
extern int  tsl_storage_read_go   (long long storage_id, int piece, long long offset, unsigned char* buf, int len);
extern int  tsl_storage_write_go  (long long storage_id, int piece, long long offset, const unsigned char* buf, int len);
extern int  tsl_storage_have_go   (long long storage_id, int piece);

/* Public installer: registers the Go callback set with the C++ side.
 * Called by lt.RegisterStorageCallbacks() on the Go side. */
int tsl_install_go_storage_callbacks(void) {
    static struct tsl_storage_callbacks cb;
    cb.open    = (void (*)(int64_t, const uint8_t*, int, int64_t))         tsl_storage_open_go;
    cb.close   = (void (*)(int64_t))                                       tsl_storage_close_go;
    cb.deleted = (void (*)(int64_t))                                       tsl_storage_deleted_go;
    cb.read    = (int  (*)(int64_t, int, int64_t, uint8_t*, int))          tsl_storage_read_go;
    cb.write   = (int  (*)(int64_t, int, int64_t, const uint8_t*, int))    tsl_storage_write_go;
    cb.have    = (int  (*)(int64_t, int))                                  tsl_storage_have_go;
    return lt_install_storage_callbacks_full(&cb);
}

/* Public clearer: drops the callback set (next session_new falls back to
 * libtorrent's default disk_io). */
int tsl_uninstall_go_storage_callbacks(void) {
    return lt_install_storage_callbacks_full(0);
}
