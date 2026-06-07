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

#include <atomic>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <memory>
#include <mutex>
#include <shared_mutex>
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

class tsl_disk_io final
    : public lt::disk_interface
    , public lt::buffer_allocator_interface
{
public:
    tsl_disk_io(lt::io_context& io,
                lt::settings_interface const& sets,
                lt::counters& cnt,
                tsl_storage_callbacks const& cb)
        : io_(io), sets_(sets), cnt_(cnt), cb_(cb) {}

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

    void abort(bool /*wait*/) override {}

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
        // success instead; at worst we feed a peer a bad block (rare, and we run
        // unchoke_slots_limit=0 so we don't upload anyway), never killing our own
        // download or stream.
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
        lt::disk_buffer_holder holder(*this, buf, r.length);
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
        int put = cb_.write(storage_id_of(s), static_cast<int>(r.piece),
                            r.start, reinterpret_cast<uint8_t const*>(buf), r.length);
        lt::storage_error err;
        if (put != r.length) err = make_io_error("write");
        lt::post(io_, [h = std::move(handler), err]() mutable { h(err); });
        return false;
    }

    void async_hash(lt::storage_index_t s,
                    lt::piece_index_t piece,
                    lt::span<lt::sha256_hash> /*v2*/,
                    lt::disk_job_flags_t /*flags*/,
                    std::function<void(lt::piece_index_t, lt::sha1_hash const&, lt::storage_error const&)> handler) override
    {
        int piece_actual = 0;
        {
            std::shared_lock<std::shared_mutex> lk(map_mu_);
            auto it = storages_.find(storage_id_of(s));
            if (it == storages_.end()) {
                lk.unlock();
                auto err = make_io_error("hash:no-storage");
                lt::post(io_, [h = std::move(handler), piece, err]() mutable {
                    h(piece, lt::sha1_hash{}, err);
                });
                return;
            }
            piece_actual = static_cast<int>(it->second.files->piece_size(piece));
        }
        std::vector<char> data(static_cast<size_t>(piece_actual));
        int got = cb_.read(storage_id_of(s), static_cast<int>(piece),
                           0, reinterpret_cast<uint8_t*>(data.data()), piece_actual);
        if (got != piece_actual) {
            auto err = make_io_error("hash:short-read");
            lt::post(io_, [h = std::move(handler), piece, err]() mutable {
                h(piece, lt::sha1_hash{}, err);
            });
            return;
        }
        lt::hasher h;
        h.update(lt::span<char const>(data.data(), piece_actual));
        auto digest = h.final();
        lt::post(io_, [hd = std::move(handler), piece, digest]() mutable {
            hd(piece, digest, lt::storage_error{});
        });
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
            h(lt::status_t::no_error, p, lt::storage_error{});
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
            h(lt::status_t::no_error, lt::storage_error{});
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

private:
    lt::io_context&               io_;
    lt::settings_interface const& sets_;
    lt::counters&                 cnt_;
    tsl_storage_callbacks         cb_;

    std::shared_mutex                              map_mu_;
    std::unordered_map<int64_t, storage_state>     storages_;
    std::atomic<int64_t>                           next_id_{1};
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
