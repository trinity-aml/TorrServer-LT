/*
 * TorrServer-LT C shim: stable ABI between Go and libtorrent (arvidn).
 *
 * Conventions:
 *  - Handles are opaque int64. 0 = invalid. Internal registry inside the shim.
 *  - Small fixed-size strings/buffers (paths, hashes, status JSON):
 *      size_t f(handle, char* buf, size_t cap);
 *    Returns the number of bytes actually needed (excluding NUL).
 *    If the return value is greater than `cap`, `buf` is untouched and the
 *    caller is expected to retry with a larger buffer.
 *  - Variable-size / unbounded outputs (alert batches, metadata bytes):
 *      char* f_alloc(handle, size_t* out_len);
 *    Returns a malloc-ed buffer; the caller MUST release it via lt_free().
 *    NULL return = error (see lt_last_error).
 *  - Integer-returning functions: 0 = OK, < 0 = error code (LT_ERR_*).
 *    A human-readable message for the last failure in the calling thread
 *    is available via lt_last_error(). The returned pointer is valid only
 *    until the next lt_* call from the same thread.
 *
 *  - JSON is used for: settings_pack, torrent_status, session_stats,
 *    alert batches, parser outputs. The shim depends on nlohmann/json
 *    (vendored under third_party/nlohmann/json.hpp).
 *  - Hot-path APIs (storage callbacks, piece priority/deadline) use plain
 *    typed C arguments to avoid per-call JSON cost.
 */

#ifndef TS_LT_SHIM_H
#define TS_LT_SHIM_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ----- types ----- */
typedef int64_t lt_session;
typedef int64_t lt_torrent;

/* ----- error codes ----- */
#define LT_OK              0
#define LT_ERR_INVALID    -1
#define LT_ERR_NOT_FOUND  -2
#define LT_ERR_TIMEOUT    -3
#define LT_ERR_IO         -4
#define LT_ERR_PARSE      -5
#define LT_ERR_NOT_IMPL   -6
#define LT_ERR_INTERNAL  -99

/* ----- piece priorities (mirror lt::download_priority_t) ----- */
#define LT_PRIO_DONT_DOWNLOAD 0
#define LT_PRIO_NORMAL        4
#define LT_PRIO_LOW           1
#define LT_PRIO_HIGH          6
#define LT_PRIO_TOP_PRIORITY  7

/* ----- alert categories (mirror lt::alert_category_t bit flags) -----
 * If the bitmask is 0, the shim picks a sensible default
 * (status | storage | error | piece_progress | block_progress | tracker).
 */
#define LT_ALERT_STATUS         0x00000001u
#define LT_ALERT_STORAGE        0x00000002u
#define LT_ALERT_ERROR          0x00000004u
#define LT_ALERT_PIECE_PROGRESS 0x00000008u
#define LT_ALERT_BLOCK_PROGRESS 0x00000010u
#define LT_ALERT_TRACKER        0x00000020u
#define LT_ALERT_PEER           0x00000040u
#define LT_ALERT_DHT            0x00000080u
#define LT_ALERT_PORT_MAPPING   0x00000100u
#define LT_ALERT_PERFORMANCE    0x00000200u
#define LT_ALERT_DEFAULT (LT_ALERT_STATUS | LT_ALERT_STORAGE | LT_ALERT_ERROR | LT_ALERT_PIECE_PROGRESS | LT_ALERT_BLOCK_PROGRESS | LT_ALERT_TRACKER)

/* ----- error reporting ----- */
/* Thread-local last error message. Valid until the next lt_* call from
 * this thread. Never returns NULL; an empty string means "no error". */
const char* lt_last_error(void);

/* Thread-local last error code (one of LT_ERR_*). Returns LT_OK if no
 * error has been recorded since the last successful operation in this
 * thread. Mainly useful for handle/pointer returning functions where the
 * code cannot be returned in-band. */
int lt_last_error_code(void);

/* ----- memory ----- */
void  lt_free(void* p);

/* ----- versions ----- */
/* "MatriX.LT-001" (or whatever the shim build was tagged with) */
size_t lt_shim_version(char* buf, size_t cap);

/* libtorrent version, e.g. "2.0.10.0" */
size_t lt_engine_version(char* buf, size_t cap);

/* ----- session lifecycle ----- */
/* settings_json may be NULL or "{}" for defaults. */
lt_session lt_session_new(const char* settings_json);

/* Apply a JSON dict to the session_pack. Unknown keys are ignored with a
 * warning recorded as last_error (but the call still returns LT_OK). */
int        lt_session_apply_settings(lt_session s, const char* settings_json);

/* P2P-format text (one range per line: desc:from-to or desc:single).
 * Empty string clears the filter. */
int        lt_session_set_ip_filter(lt_session s, const char* p2p_text);

/* Bitmask of LT_ALERT_*. 0 = use LT_ALERT_DEFAULT. */
int        lt_session_set_alert_mask(lt_session s, uint32_t mask);

int        lt_session_destroy(lt_session s);

/* ----- torrent lifecycle -----
 * `link` may be: magnet URI, 40-hex info hash, http(s) URL, or empty
 * (then info_bytes must be non-NULL).
 *
 * Returns torrent handle on success, 0 on error (see lt_last_error).
 */
lt_torrent lt_session_add_torrent(
    lt_session s,
    const char* link,
    const uint8_t* info_bytes, size_t info_len,
    const char* trackers_csv,
    const char* save_path,
    int paused);

int lt_torrent_remove(lt_session s, lt_torrent t, int delete_files);
int lt_torrent_pause(lt_torrent t);
int lt_torrent_resume(lt_torrent t);
int lt_torrent_force_recheck(lt_torrent t);

/* ----- metadata accessors ----- */
/* 1 if metadata is present, 0 if not yet downloaded, < 0 on error */
int     lt_torrent_have_metadata(lt_torrent t);

/* Raw .torrent bytes (info dict). NULL if metadata not yet received. */
char*   lt_torrent_metadata_alloc(lt_torrent t, size_t* out_len);

int     lt_torrent_num_files(lt_torrent t);
size_t  lt_torrent_file_path(lt_torrent t, int idx, char* buf, size_t cap);
int64_t lt_torrent_file_size(lt_torrent t, int idx);
int64_t lt_torrent_file_offset(lt_torrent t, int idx);
int     lt_torrent_num_pieces(lt_torrent t);
int64_t lt_torrent_piece_length(lt_torrent t);
int64_t lt_torrent_total_size(lt_torrent t);

/* For magnet links without metadata, the display_name (dn=) parameter. */
size_t  lt_torrent_display_name(lt_torrent t, char* buf, size_t cap);

/* 40-char lowercase hex (v1 SHA-1). */
size_t  lt_torrent_info_hash_hex(lt_torrent t, char* buf, size_t cap);

/* ----- priorities & streaming ----- */
int lt_torrent_set_piece_priority(lt_torrent t, int piece_idx, int prio);
int lt_torrent_set_piece_deadline(lt_torrent t, int piece_idx, int deadline_ms, int alert_when_ready);
int lt_torrent_clear_piece_deadlines(lt_torrent t);
int lt_torrent_set_file_priority(lt_torrent t, int file_idx, int prio);

/* ----- status & stats -----
 * status_json output schema (subset used by Go state.TorrentStatus):
 *   {
 *     "name": str, "info_hash": hex,
 *     "state": str (downloading/seeding/...), "is_paused": bool, "is_finished": bool,
 *     "progress": float (0..1), "total_done": int64, "total_wanted": int64,
 *     "download_rate": int, "upload_rate": int,
 *     "num_peers": int, "num_seeds": int, "list_peers": int, "list_seeds": int,
 *     "connect_candidates": int, "total_payload_download": int64, "total_payload_upload": int64,
 *     "pieces_have": int, "num_pieces": int, "piece_length": int
 *   }
 */
size_t lt_torrent_status_json(lt_torrent t, char* buf, size_t cap);

/* session_stats: per-libtorrent BEP-37-style counters + key gauges. */
char*  lt_session_stats_json_alloc(lt_session s, size_t* out_len);

/* ----- alert pump -----
 * Blocks up to timeout_ms (0 = non-blocking poll, < 0 = wait forever).
 * Returns approximate number of available alerts (libtorrent's hint) or
 * < 0 on error.
 */
int    lt_session_wait_alert(lt_session s, int timeout_ms);

/* Pops ALL accumulated alerts and returns them as a JSON array.
 *
 * Each element: { "type": str, "category": int, "what": str, "message": str,
 *                 "torrent": int64_t (if applicable), ...type-specific fields }
 * Empty array "[]" is a valid result. */
char*  lt_session_pop_alerts_json_alloc(lt_session s, size_t* out_len);

/* ----- storage callbacks (registered once, process-global) -----
 * Wired up in Etap 4. For now lt_register_storage_callbacks is a stub
 * that records the pointers but the actual custom_storage hookup into
 * libtorrent's disk_interface is not active.
 */
typedef int (*lt_storage_read_fn) (lt_torrent t, int piece, int64_t off, uint8_t* buf, int len);
typedef int (*lt_storage_write_fn)(lt_torrent t, int piece, int64_t off, const uint8_t* buf, int len);
typedef int (*lt_storage_have_fn) (lt_torrent t, int piece);

int lt_register_storage_callbacks(
    lt_storage_read_fn  read_cb,
    lt_storage_write_fn write_cb,
    lt_storage_have_fn  have_cb);

/* ----- parsers (utility) -----
 * All return JSON-alloc:
 *   { "info_hash": hex, "display_name": str,
 *     "trackers": [str, ...],
 *     "info_bytes_b64": str (only if .torrent files were parsed) }
 */
char* lt_parse_magnet_alloc       (const char* uri,                  size_t* out_len);
char* lt_parse_torrent_file_alloc (const char* path,                 size_t* out_len);
char* lt_parse_torrent_bytes_alloc(const uint8_t* buf, size_t len,   size_t* out_len);

#ifdef __cplusplus
}
#endif

#endif
