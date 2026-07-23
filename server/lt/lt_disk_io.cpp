// libtorrent custom disk_interface implementation.
//
// Backs every piece read/write/hash with a Go callback. New torrents get
// a shim-minted storage_id; Go uses that id to associate operations with
// the right per-torrent Cache. The translation between libtorrent's
// `peer_request` (piece + offset + length) and the Go-side Piece layout
// is a 1:1 pass-through.
//
// For Etap 4.1 the storage is purely a delegation layer: libtorrent
// calls us, we ferry bytes between its disk thread pool and the Go
// Cache. The Go side may keep the bytes in RAM (MemPiece) or on disk
// (DiskPiece, Etap 4.2) — that decision belongs in Go, not here.

#include "lt_disk_io.h"
#include "lt_shim.h"

#include <libtorrent/disk_buffer_holder.hpp>
#include <libtorrent/disk_interface.hpp>
#include <libtorrent/file_storage.hpp>
#include <libtorrent/hasher.hpp>
#include <libtorrent/io_context.hpp>
#include <libtorrent/peer_request.hpp>
#include <libtorrent/session_params.hpp>
#include <libtorrent/storage_defs.hpp>
#include <libtorrent/units.hpp>
#include <libtorrent/version.hpp>

#include <atomic>
#include <condition_variable>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <functional>
#include <memory>
#include <mutex>
#include <shared_mutex>
#include <thread>
#include <unordered_map>
#include <vector>

namespace lt = libtorrent;

// ============================================================================
// global Go-callback registry
// ============================================================================
namespace {

std::mutex                g_cb_mu;
tsl_storage_callbacks     g_cb{};
bool                      g_cb_set = false;

bool callbacks_complete(tsl_storage_callbacks const& c) {
    return c.open && c.close && c.deleted && c.read && c.write && c.have;
}

} // namespace

extern "C" int lt_install_storage_callbacks_full(tsl_storage_callbacks const* cb) {
    std::lock_guard<std::mutex> lk(g_cb_mu);
    if (!cb) {
        g_cb = {};
        g_cb_set = false;
        return LT_OK;
    }
    if (!callbacks_complete(*cb)) return LT_ERR_INVALID;
    g_cb = *cb;
    g_cb_set = true;
    return LT_OK;
}

bool tsl_has_storage_callbacks() {
    std::lock_guard<std::mutex> lk(g_cb_mu);
    return g_cb_set;
}

namespace {
// Snapshot of the registered callbacks. Holds a local copy at session
// creation time so a later lt_install_storage_callbacks_full(nullptr) on
// another thread can't pull the rug out.
tsl_storage_callbacks current_callbacks() {
    std::lock_guard<std::mutex> lk(g_cb_mu);
    return g_cb;
}
} // namespace

// ============================================================================
// helpers
// ============================================================================
namespace {

inline lt::storage_error make_io_error(char const* op) {
    lt::storage_error se;
    se.ec = lt::error_code(boost::system::errc::io_error, lt::system_category());
    se.operation = lt::operation_t::partfile_read;
    (void)op;
    return se;
}

inline std::string sha1_raw_from(lt::sha1_hash const& h) {
    return std::string(reinterpret_cast<char const*>(h.data()), 20);
}

// libtorrent's storage_index_t is aux::strong_typedef wrapping either
// `int` (Ubuntu's 2.0.10 packaged headers) or `unsigned int` (upstream
// RC_2_0 git). Use the public underlying_index_t meta-function so the
// shim compiles against both.
inline int64_t storage_id_of(lt::storage_index_t s) {
    using U = typename lt::aux::underlying_index_t<lt::storage_index_t>::type;
    return static_cast<int64_t>(static_cast<U>(s));
}

// libtorrent 2.1 turned status_t into a flags type: "no error" is the empty
// flag set and the bits live in lt::disk_status::*. 2.0 spelled the same
// thing status_t::no_error. One name for both so the handlers read clean.
#if LIBTORRENT_VERSION_NUM >= 20100
constexpr lt::status_t tsl_status_ok{};
#else
constexpr lt::status_t tsl_status_ok = lt::status_t::no_error;
#endif

} // namespace

// ============================================================================
// the custom disk_interface
// ============================================================================
namespace {

struct storage_state {
    std::shared_ptr<lt::file_storage const> files;
    int          num_pieces   = 0;
    int          piece_length = 0;
    lt::sha1_hash info_hash;
};

// One queued piece-hash request handed to the hashing thread pool.
struct hash_job {
    lt::storage_index_t s;
    lt::piece_index_t   piece;
    std::function<void(lt::piece_index_t, lt::sha1_hash const&, lt::storage_error const&)> handler;
};

class tsl_disk_io final
    : public lt::disk_interface
    , public lt::buffer_allocator_interface
{
public:
    tsl_disk_io(lt::io_context& io,
                lt::settings_interface const& sets,
                lt::counters& cnt,
                tsl_storage_callbacks const& cb)
        : io_(io), sets_(sets), cnt_(cnt), cb_(cb)
    {
        // Hashing thread pool: SHA-1 every completed piece OFF the session's
        // network thread, so it never blocks peer I/O / the picker (and runs in
        // parallel across cores). Sized to the core count, leaving one core for
        // the network thread, capped at 4 (SHA-1 saturates memory bandwidth past
        // a few threads). NB: libtorrent's hashing_threads/aio_threads settings
        // do NOT apply here — those configure its BUILT-IN disk_io, which this
        // custom disk_interface replaces, so the pool is sized ourselves.
        unsigned hw = std::thread::hardware_concurrency();
        if (hw == 0) hw = 2; // unknown — assume dual-core
        int n = static_cast<int>(hw) - 1;
        if (n < 1) n = 1;
        if (n > 4) n = 4;
        for (int i = 0; i < n; ++i)
            hash_threads_.emplace_back([this]{ hash_worker(); });
    }

    ~tsl_disk_io() override { stop_hash_pool(); }

    // ----- storage lifecycle -----

    lt::storage_holder new_torrent(lt::storage_params const& p,
                                   std::shared_ptr<void> const&) override
    {
        int64_t idx = next_id_++;

        storage_state ss;
        ss.files = std::make_shared<lt::file_storage>(p.files);
        ss.num_pieces = ss.files->num_pieces();
        ss.piece_length = ss.files->piece_length();
        ss.info_hash = p.info_hash;

        {
            std::unique_lock<std::shared_mutex> lk(map_mu_);
            storages_.emplace(idx, std::move(ss));
        }

        auto raw = sha1_raw_from(p.info_hash);
        cb_.open(idx, reinterpret_cast<uint8_t const*>(raw.data()),
                 ss.num_pieces, ss.piece_length);

        auto storage_idx = lt::storage_index_t(static_cast<int>(idx));
        return lt::storage_holder(storage_idx, *this);
    }

    void remove_torrent(lt::storage_index_t s) override {
        int64_t idx = storage_id_of(s);
        {
            std::unique_lock<std::shared_mutex> lk(map_mu_);
            storages_.erase(idx);
        }
        cb_.close(idx);
    }

    void abort(bool /*wait*/) override { stop_hash_pool(); }

    // ----- I/O -----

    void async_read(lt::storage_index_t s,
                    lt::peer_request const& r,
                    std::function<void(lt::disk_buffer_holder, lt::storage_error const&)> handler,
                    lt::disk_job_flags_t /*flags*/ = {}) override
    {
        char* buf = static_cast<char*>(std::malloc(static_cast<size_t>(r.length)));
        if (!buf) {
            auto err = make_io_error("read");
            lt::post(io_, [h = std::move(handler), err]() mutable {
                h(lt::disk_buffer_holder{}, err);
            });
            return;
        }
        int got = cb_.read(storage_id_of(s), static_cast<int>(r.piece),
                           r.start, reinterpret_cast<uint8_t*>(buf), r.length);
        // A short/empty read means the piece was evicted from the streaming
        // cache (we only keep the reader's window + recently-played pieces).
        // async_read is libtorrent's UPLOAD path — our own HTTP Reader reads the
        // cache directly and hashing uses async_hash — so a miss here must NOT
        // fault the torrent: returning a storage_error makes libtorrent treat it
        // as disk corruption and pause playback. Zero-fill the gap and report
        // success instead; at worst we feed a peer a bad block, never killing our
        // own download or stream.
        //
        // On eviction we now also call WeDontHave (piece_picker un-have) so
        // libtorrent stops advertising evicted pieces and peers stop requesting
        // them — but that un-have is posted to the network thread, so there is a
        // brief window where a request for a just-evicted piece can still arrive.
        // This zero-fill safety net covers that race.
        if (got < 0) got = 0;
        if (got < r.length) {
            std::memset(buf + got, 0, static_cast<size_t>(r.length - got));
        }
        lt::storage_error err;
        // Do the I/O inline (like posix_disk_io) but deliver the completion
        // handler via the session's io_context. libtorrent requires disk
        // handlers to be posted, not invoked re-entrantly — calling them
        // synchronously breaks its piece-completion bookkeeping (it never
        // schedules async_hash, so pieces never finish).
#if LIBTORRENT_VERSION_NUM >= 20100
        lt::disk_buffer_holder holder(*this, buf); // 2.1 dropped the size arg
#else
        lt::disk_buffer_holder holder(*this, buf, r.length);
#endif
        lt::post(io_, [h = std::move(handler), holder = std::move(holder), err]() mutable {
            h(std::move(holder), err);
        });
    }

    bool async_write(lt::storage_index_t s,
                     lt::peer_request const& r,
                     char const* buf,
                     std::shared_ptr<lt::disk_observer> /*o*/,
                     std::function<void(lt::storage_error const&)> handler,
                     lt::disk_job_flags_t /*flags*/ = {}) override
    {
        // cb_.write copies the block into the cache synchronously, so `buf`
        // need not outlive this call; only the completion handler is deferred.
        (void)cb_.write(storage_id_of(s), static_cast<int>(r.piece),
                        r.start, reinterpret_cast<uint8_t const*>(buf), r.length);
        // Never fault on a short write. This is an in-RAM cache: a short copy only
        // happens if the piece was evicted out from under us (the same eviction race
        // async_read guards). Returning a storage_error makes libtorrent treat it as
        // fatal disk corruption and pause the torrent; instead report success. A block
        // that didn't stick leaves the piece incomplete, so libtorrent re-requests it
        // (or the hash mismatch below forces a re-download) — never a dead stream.
        lt::storage_error err;
        lt::post(io_, [h = std::move(handler), err]() mutable { h(err); });
        return false;
    }

    void async_hash(lt::storage_index_t s,
                    lt::piece_index_t piece,
                    lt::span<lt::sha256_hash> /*v2*/,
                    lt::disk_job_flags_t /*flags*/,
                    std::function<void(lt::piece_index_t, lt::sha1_hash const&, lt::storage_error const&)> handler) override
    {
        // Hand the read + SHA-1 to a worker so it runs off the network thread.
        // If the pool is already stopped (shutdown), hash inline so the
        // completion handler always fires and libtorrent never wedges.
        hash_job job{s, piece, std::move(handler)};
        {
            std::lock_guard<std::mutex> lk(hash_mu_);
            if (!hash_stop_ && !hash_threads_.empty()) {
                hash_queue_.push_back(std::move(job));
                hash_cv_.notify_one();
                return;
            }
        }
        run_hash(std::move(job));
    }

    void async_hash2(lt::storage_index_t,
                     lt::piece_index_t piece,
                     int /*offset*/,
                     lt::disk_job_flags_t,
                     std::function<void(lt::piece_index_t, lt::sha256_hash const&, lt::storage_error const&)> handler) override
    {
        lt::storage_error se;
        se.ec = lt::error_code(boost::system::errc::function_not_supported, lt::system_category());
        lt::post(io_, [h = std::move(handler), piece, se]() mutable {
            h(piece, lt::sha256_hash{}, se);
        });
    }

    // ----- async maintenance ----- (handlers posted to io_, see async_read)

    void async_move_storage(lt::storage_index_t,
                            std::string p,
                            lt::move_flags_t,
                            std::function<void(lt::status_t, std::string const&, lt::storage_error const&)> handler) override
    {
        lt::post(io_, [h = std::move(handler), p = std::move(p)]() mutable {
            h(tsl_status_ok, p, lt::storage_error{});
        });
    }

    void async_release_files(lt::storage_index_t, std::function<void()> handler) override {
        lt::post(io_, std::move(handler));
    }

    void async_delete_files(lt::storage_index_t s,
                            lt::remove_flags_t,
                            std::function<void(lt::storage_error const&)> handler) override
    {
        cb_.deleted(storage_id_of(s));
        lt::post(io_, [h = std::move(handler)]() mutable { h(lt::storage_error{}); });
    }

    void async_check_files(lt::storage_index_t /*s*/,
                           lt::add_torrent_params const* /*resume*/,
                           lt::aux::vector<std::string, lt::file_index_t> /*links*/,
                           std::function<void(lt::status_t, lt::storage_error const&)> handler) override
    {
        // 4.1: nothing on disk yet — let libtorrent treat all pieces as missing.
        // 4.2 hooks `have()` callback to populate the bitmap before this step.
        lt::post(io_, [h = std::move(handler)]() mutable {
            h(tsl_status_ok, lt::storage_error{});
        });
    }

    void async_rename_file(lt::storage_index_t,
                           lt::file_index_t idx,
                           std::string name,
                           std::function<void(std::string const&, lt::file_index_t, lt::storage_error const&)> handler) override
    {
        lt::post(io_, [h = std::move(handler), idx, name = std::move(name)]() mutable {
            h(name, idx, lt::storage_error{});
        });
    }

    void async_stop_torrent(lt::storage_index_t, std::function<void()> handler) override {
        lt::post(io_, std::move(handler));
    }

    void async_set_file_priority(lt::storage_index_t,
                                 lt::aux::vector<lt::download_priority_t, lt::file_index_t> prio,
                                 std::function<void(lt::storage_error const&, lt::aux::vector<lt::download_priority_t, lt::file_index_t>)> handler) override
    {
        lt::post(io_, [h = std::move(handler), prio = std::move(prio)]() mutable {
            h(lt::storage_error{}, std::move(prio));
        });
    }

    void async_clear_piece(lt::storage_index_t,
                           lt::piece_index_t idx,
                           std::function<void(lt::piece_index_t)> handler) override
    {
        lt::post(io_, [h = std::move(handler), idx]() mutable { h(idx); });
    }

    // ----- accounting / control -----

    void update_stats_counters(lt::counters&) const override {}
    std::vector<lt::open_file_state> get_status(lt::storage_index_t) const override { return {}; }
    void submit_jobs() override {}
    void settings_updated() override {}

    // ----- buffer_allocator_interface -----

    void free_disk_buffer(char* buf) override { std::free(buf); }

#if LIBTORRENT_VERSION_NUM >= 20100
    // New pure virtual in 2.1: batched frees from the peer connection loop.
    void free_multiple_buffers(lt::span<char*> bufs) override {
        for (char* b : bufs) std::free(b);
    }
#endif

private:
    // ----- hashing thread pool -----

    // run_hash does the actual work: read the piece out of the Go cache and
    // SHA-1 it, posting the result (or an error) back through io_. Runs on a
    // worker thread (or inline if the pool is gone). Same logic the old inline
    // async_hash had — cb_.read is RLock-guarded on the Go side and lt::post is
    // thread-safe, so several workers can hash different pieces concurrently.
    void run_hash(hash_job job) {
        int piece_actual = 0;
        {
            std::shared_lock<std::shared_mutex> lk(map_mu_);
            auto it = storages_.find(storage_id_of(job.s));
            if (it == storages_.end()) {
                lk.unlock();
                auto err = make_io_error("hash:no-storage");
                lt::post(io_, [h = std::move(job.handler), piece = job.piece, err]() mutable {
                    h(piece, lt::sha1_hash{}, err);
                });
                return;
            }
            piece_actual = static_cast<int>(it->second.files->piece_size(job.piece));
        }
        std::vector<char> data(static_cast<size_t>(piece_actual)); // zero-initialised
        int got = cb_.read(storage_id_of(job.s), static_cast<int>(job.piece),
                           0, reinterpret_cast<uint8_t*>(data.data()), piece_actual);
        // A short read means the piece was evicted from the RAM cache between its
        // last write and this hash (the bounded hash pool can lag the reap). Do NOT
        // fault: a storage_error here is read as fatal disk corruption and pauses the
        // torrent. Instead hash whatever we read, zero-padded — the digest then won't
        // match the expected hash, so libtorrent simply re-downloads the piece. An
        // in-RAM cache must never be able to pause the torrent over an eviction.
        (void)got;
        lt::hasher h;
        h.update(lt::span<char const>(data.data(), piece_actual));
        auto digest = h.final();
        lt::post(io_, [hd = std::move(job.handler), piece = job.piece, digest]() mutable {
            hd(piece, digest, lt::storage_error{});
        });
    }

    void hash_worker() {
        for (;;) {
            std::unique_lock<std::mutex> lk(hash_mu_);
            hash_cv_.wait(lk, [this]{ return hash_stop_ || !hash_queue_.empty(); });
            if (hash_queue_.empty()) return; // stop requested and queue drained
            hash_job job = std::move(hash_queue_.front());
            hash_queue_.pop_front();
            lk.unlock();
            run_hash(std::move(job));
        }
    }

    // Drain + join. Workers finish whatever is queued (so every completion still
    // fires), then exit. Idempotent: called from both abort() and the destructor.
    void stop_hash_pool() {
        {
            std::lock_guard<std::mutex> lk(hash_mu_);
            if (hash_stop_) return;
            hash_stop_ = true;
        }
        hash_cv_.notify_all();
        for (auto& t : hash_threads_)
            if (t.joinable()) t.join();
        hash_threads_.clear();
    }

    lt::io_context&               io_;
    lt::settings_interface const& sets_;
    lt::counters&                 cnt_;
    tsl_storage_callbacks         cb_;

    std::shared_mutex                              map_mu_;
    std::unordered_map<int64_t, storage_state>     storages_;
    std::atomic<int64_t>                           next_id_{1};

    // hashing thread pool (see the constructor for sizing)
    std::vector<std::thread> hash_threads_;
    std::mutex               hash_mu_;
    std::condition_variable  hash_cv_;
    std::deque<hash_job>     hash_queue_;
    bool                     hash_stop_ = false;
};

// Factory used by session_params.disk_io_constructor. Captures the
// callback snapshot taken at session_new time via a function-local
// static — see tsl_install_disk_io_on().
tsl_storage_callbacks g_session_cb{};

std::unique_ptr<lt::disk_interface> tsl_make_disk_io(
    lt::io_context& io, lt::settings_interface const& s, lt::counters& cnt)
{
    return std::make_unique<tsl_disk_io>(io, s, cnt, g_session_cb);
}

} // namespace

void tsl_install_disk_io_on(lt::session_params& params) {
    if (!tsl_has_storage_callbacks()) return;
    g_session_cb = current_callbacks();
    params.disk_io_constructor = &tsl_make_disk_io;
}
