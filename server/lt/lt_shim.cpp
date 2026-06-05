// TorrServer-LT C shim implementation.
// See lt_shim.h for the contract.
//
// libtorrent (arvidn) 2.x API only — v1 SHA-1 torrents.
// Custom disk_io (Etap 4.1) lives in lt_disk_io.cpp and is wired in when
// Go has installed storage callbacks via lt_install_storage_callbacks_full.

#include "lt_shim.h"
#include "lt_disk_io.h"

#include "third_party/nlohmann/json.hpp"

#include <libtorrent/add_torrent_params.hpp>
#include <libtorrent/alert.hpp>
#include <libtorrent/alert_types.hpp>
#include <libtorrent/error_code.hpp>
#include <libtorrent/ip_filter.hpp>
#include <libtorrent/magnet_uri.hpp>
#include <libtorrent/session.hpp>
#include <libtorrent/session_params.hpp>
#include <libtorrent/session_stats.hpp>
#include <libtorrent/settings_pack.hpp>
#include <libtorrent/sha1_hash.hpp>
#include <libtorrent/torrent_handle.hpp>
#include <libtorrent/torrent_info.hpp>
#include <libtorrent/torrent_status.hpp>
#include <libtorrent/version.hpp>

// Implemented in lt_disk_io.cpp; declared here after all libtorrent
// headers have been seen so the session_params type is unambiguous.
extern void tsl_install_disk_io_on(libtorrent::session_params& params);

#include <atomic>
#include <chrono>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <memory>
#include <mutex>
#include <shared_mutex>
#include <sstream>
#include <string>
#include <unordered_map>
#include <utility>
#include <vector>

namespace lt = libtorrent;
using json = nlohmann::json;

// ============================================================================
// thread-local last error
// ============================================================================
namespace {

thread_local std::string g_last_error;
thread_local int         g_last_error_code = LT_OK;

inline int set_err(int code, std::string msg) {
    g_last_error = std::move(msg);
    g_last_error_code = code;
    return code;
}

inline int set_err_ec(int code, lt::error_code const& ec) {
    g_last_error = ec.message();
    g_last_error_code = code;
    return code;
}

inline char* alloc_string(std::string const& s, size_t* out_len) {
    if (out_len) *out_len = s.size();
    char* p = static_cast<char*>(std::malloc(s.size() + 1));
    if (!p) {
        set_err(LT_ERR_INTERNAL, "out of memory");
        return nullptr;
    }
    std::memcpy(p, s.data(), s.size());
    p[s.size()] = '\0';
    return p;
}

inline size_t copy_string(std::string const& s, char* buf, size_t cap) {
    if (buf && cap > s.size()) {
        std::memcpy(buf, s.data(), s.size());
        buf[s.size()] = '\0';
    }
    return s.size();
}

// 20-byte sha1 → 40-char lowercase hex
inline std::string sha1_hex(lt::sha1_hash const& h) {
    static const char* hex = "0123456789abcdef";
    std::string out(40, '0');
    auto const* p = reinterpret_cast<unsigned char const*>(h.data());
    for (int i = 0; i < 20; ++i) {
        out[i * 2]     = hex[p[i] >> 4];
        out[i * 2 + 1] = hex[p[i] & 0x0f];
    }
    return out;
}

// 40-char hex → sha1_hash; returns false if input is not exactly 40 hex chars.
inline bool hex_to_sha1(std::string const& hex, lt::sha1_hash& out) {
    if (hex.size() != 40) return false;
    unsigned char buf[20] = {0};
    auto nyb = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return 10 + c - 'a';
        if (c >= 'A' && c <= 'F') return 10 + c - 'A';
        return -1;
    };
    for (int i = 0; i < 20; ++i) {
        int hi = nyb(hex[i * 2]);
        int lo = nyb(hex[i * 2 + 1]);
        if (hi < 0 || lo < 0) return false;
        buf[i] = static_cast<unsigned char>((hi << 4) | lo);
    }
    out = lt::sha1_hash(reinterpret_cast<char const*>(buf));
    return true;
}

inline std::vector<std::string> split_csv(std::string const& csv) {
    std::vector<std::string> out;
    std::string cur;
    for (char c : csv) {
        if (c == ',') {
            if (!cur.empty()) out.push_back(std::move(cur));
            cur.clear();
        } else if (c != '\r' && c != '\n') {
            cur.push_back(c);
        }
    }
    if (!cur.empty()) out.push_back(std::move(cur));
    return out;
}

} // namespace

// ============================================================================
// session and torrent registries
// ============================================================================
namespace {

struct session_slot {
    std::unique_ptr<lt::session> s;
    std::mutex pump_mu; // serializes wait_alert+pop_alerts on this session
};

std::shared_mutex g_sess_mu;
std::unordered_map<int64_t, std::shared_ptr<session_slot>> g_sessions;
std::atomic<int64_t> g_next_sess{1};

std::shared_mutex g_torr_mu;
std::unordered_map<int64_t, lt::torrent_handle> g_torrents;
std::unordered_map<std::string, int64_t> g_hash2id;
std::atomic<int64_t> g_next_torr{1};

std::shared_ptr<session_slot> get_session(lt_session id) {
    std::shared_lock<std::shared_mutex> lk(g_sess_mu);
    auto it = g_sessions.find(id);
    if (it == g_sessions.end()) return nullptr;
    return it->second;
}

lt::torrent_handle get_torrent(lt_torrent id) {
    std::shared_lock<std::shared_mutex> lk(g_torr_mu);
    auto it = g_torrents.find(id);
    if (it == g_torrents.end()) return lt::torrent_handle();
    return it->second;
}

int64_t register_torrent(lt::torrent_handle const& h) {
    if (!h.is_valid()) return 0;
    auto hashes = h.info_hashes();
    std::string hex = sha1_hex(hashes.v1);
    std::unique_lock<std::shared_mutex> lk(g_torr_mu);
    auto exist = g_hash2id.find(hex);
    if (exist != g_hash2id.end()) return exist->second;
    int64_t id = g_next_torr++;
    g_torrents.emplace(id, h);
    g_hash2id.emplace(std::move(hex), id);
    return id;
}

int64_t lookup_torrent_id(lt::torrent_handle const& h) {
    if (!h.is_valid()) return 0;
    auto hashes = h.info_hashes();
    std::string hex = sha1_hex(hashes.v1);
    std::shared_lock<std::shared_mutex> lk(g_torr_mu);
    auto it = g_hash2id.find(hex);
    if (it == g_hash2id.end()) return 0;
    return it->second;
}

void unregister_torrent(lt_torrent id) {
    std::unique_lock<std::shared_mutex> lk(g_torr_mu);
    auto it = g_torrents.find(id);
    if (it == g_torrents.end()) return;
    if (it->second.is_valid()) {
        auto hashes = it->second.info_hashes();
        g_hash2id.erase(sha1_hex(hashes.v1));
    }
    g_torrents.erase(it);
}

} // namespace

// ============================================================================
// settings JSON ↔ settings_pack
// ============================================================================
namespace {

void json_into_settings(json const& j, lt::settings_pack& sp, std::string* warnings) {
    if (!j.is_object()) return;
    for (auto it = j.begin(); it != j.end(); ++it) {
        std::string name = it.key();
        int id = lt::setting_by_name(name);
        if (id < 0) {
            if (warnings) {
                if (!warnings->empty()) warnings->push_back(';');
                *warnings += "unknown setting: " + name;
            }
            continue;
        }
        int const type_mask = id & lt::settings_pack::type_mask;
        try {
            if (type_mask == lt::settings_pack::string_type_base) {
                if (it->is_string()) sp.set_str(id, it->get<std::string>());
            } else if (type_mask == lt::settings_pack::int_type_base) {
                if (it->is_number_integer()) sp.set_int(id, it->get<int>());
                else if (it->is_number_float()) sp.set_int(id, static_cast<int>(it->get<double>()));
            } else if (type_mask == lt::settings_pack::bool_type_base) {
                if (it->is_boolean()) sp.set_bool(id, it->get<bool>());
                else if (it->is_number_integer()) sp.set_bool(id, it->get<int>() != 0);
            }
        } catch (std::exception const& e) {
            if (warnings) {
                if (!warnings->empty()) warnings->push_back(';');
                *warnings += std::string("bad value for ") + name + ": " + e.what();
            }
        }
    }
}

} // namespace

// ============================================================================
// ip_filter (P2P-format text → lt::ip_filter)
// ============================================================================
namespace {

lt::ip_filter parse_p2p_filter(std::string const& text) {
    lt::ip_filter f;
    std::istringstream in(text);
    std::string line;
    while (std::getline(in, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        std::string s;
        for (char c : line) {
            if (c == '\t' || c == ' ') continue;
            s.push_back(c);
        }
        if (s.empty() || s[0] == '#') continue;
        auto colon = s.find_last_of(':');
        if (colon == std::string::npos) continue;
        std::string rng = s.substr(colon + 1);
        auto dash = rng.find('-');
        lt::error_code ec1, ec2;
        lt::address first, last;
        if (dash == std::string::npos) {
            first = lt::make_address(rng, ec1);
            last = first;
        } else {
            first = lt::make_address(rng.substr(0, dash), ec1);
            last  = lt::make_address(rng.substr(dash + 1), ec2);
        }
        if (ec1 || ec2) continue;
        if (first.is_v4() != last.is_v4()) continue;
        f.add_rule(first, last, lt::ip_filter::blocked);
    }
    return f;
}

} // namespace

// ============================================================================
// status → JSON
// ============================================================================
namespace {

std::string state_name(lt::torrent_status::state_t st) {
    switch (st) {
        case lt::torrent_status::checking_files:    return "checking_files";
        case lt::torrent_status::downloading_metadata: return "downloading_metadata";
        case lt::torrent_status::downloading:       return "downloading";
        case lt::torrent_status::finished:          return "finished";
        case lt::torrent_status::seeding:           return "seeding";
        case lt::torrent_status::checking_resume_data: return "checking_resume_data";
        default: return "unknown";
    }
}

json status_to_json(lt::torrent_handle const& h) {
    json j;
    if (!h.is_valid()) return j;
    auto st = h.status();
    j["name"]         = st.name;
    auto hashes = h.info_hashes();
    j["info_hash"]    = sha1_hex(hashes.v1);
    j["state"]        = state_name(st.state);
    j["is_finished"]  = st.is_finished;
    j["progress"]     = st.progress;
    j["total_done"]   = st.total_done;
    j["total_wanted"] = st.total_wanted;
    j["download_rate"]   = st.download_rate;
    j["upload_rate"]     = st.upload_rate;
    j["num_peers"]       = st.num_peers;
    j["num_seeds"]       = st.num_seeds;
    j["list_peers"]      = st.list_peers;
    j["list_seeds"]      = st.list_seeds;
    j["connect_candidates"]    = st.connect_candidates;
    j["total_payload_download"] = st.total_payload_download;
    j["total_payload_upload"]   = st.total_payload_upload;
    j["total_download"]         = st.total_download;
    j["total_upload"]           = st.total_upload;
    j["num_pieces"]             = st.num_pieces;
    auto ti = h.torrent_file();
    if (ti) {
        j["piece_length"] = ti->piece_length();
        j["total_size"]   = ti->total_size();
        j["has_metadata"] = true;
    } else {
        j["piece_length"] = 0;
        j["total_size"]   = 0;
        j["has_metadata"] = false;
    }
    return j;
}

} // namespace

// ============================================================================
// alert → JSON
// ============================================================================
namespace {

json alert_to_json(lt::alert const* a) {
    json j;
    j["type"]     = a->what();
    j["category"] = static_cast<uint64_t>(static_cast<std::uint32_t>(a->category()));
    j["message"]  = a->message();

    // torrent_alert is an abstract base — its `alert_type` constant is
    // deprecated and may be absent (libtorrent built with
    // -Ddeprecated-functions=OFF). Walk the hierarchy via RTTI instead.
    if (auto const* ta = dynamic_cast<lt::torrent_alert const*>(a)) {
        int64_t id = lookup_torrent_id(ta->handle);
        if (id != 0) j["torrent"] = id;
        if (ta->handle.is_valid()) {
            j["torrent_hash"] = sha1_hex(ta->handle.info_hashes().v1);
        }
    }

    if (auto const* fa = lt::alert_cast<lt::piece_finished_alert>(a)) {
        j["piece"] = static_cast<int>(fa->piece_index);
    } else if (auto const* fa = lt::alert_cast<lt::block_finished_alert>(a)) {
        j["piece"] = static_cast<int>(fa->piece_index);
        j["block"] = fa->block_index;
    } else if (auto const* fa = lt::alert_cast<lt::file_completed_alert>(a)) {
        j["file"] = static_cast<int>(fa->index);
    } else if (auto const* fa = lt::alert_cast<lt::hash_failed_alert>(a)) {
        j["piece"] = static_cast<int>(fa->piece_index);
    } else if (auto const* fa = lt::alert_cast<lt::tracker_reply_alert>(a)) {
        j["url"]   = std::string(fa->tracker_url());
        j["peers"] = fa->num_peers;
    } else if (auto const* fa = lt::alert_cast<lt::tracker_error_alert>(a)) {
        j["url"]   = std::string(fa->tracker_url());
        j["error"] = fa->error.message();
    } else if (auto const* fa = lt::alert_cast<lt::torrent_error_alert>(a)) {
        j["error"] = fa->error.message();
    } else if (auto const* fa = lt::alert_cast<lt::file_error_alert>(a)) {
        j["error"] = fa->error.message();
        j["file"]  = std::string(fa->filename());
    }
    return j;
}

} // namespace

// ============================================================================
// public C ABI
// ============================================================================

#define WRAP_BEGIN try {
#define WRAP_END(retcode) } catch (std::exception const& e) { \
    return set_err(LT_ERR_INTERNAL, e.what()); \
} catch (...) { \
    return set_err(LT_ERR_INTERNAL, "unknown C++ exception"); \
}

extern "C" {

// ----- error reporting / memory / version -----

const char* lt_last_error(void) {
    return g_last_error.c_str();
}

int lt_last_error_code(void) {
    return g_last_error_code;
}

void lt_free(void* p) {
    std::free(p);
}

size_t lt_shim_version(char* buf, size_t cap) {
    static const std::string ver = "MatriX.LT-001";
    return copy_string(ver, buf, cap);
}

size_t lt_engine_version(char* buf, size_t cap) {
    static const std::string ver = LIBTORRENT_VERSION;
    return copy_string(ver, buf, cap);
}

// ----- session lifecycle -----

lt_session lt_session_new(const char* settings_json) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        lt::session_params params;
        params.settings.set_int(lt::settings_pack::alert_mask, LT_ALERT_DEFAULT);

        if (settings_json && *settings_json) {
            try {
                auto j = json::parse(settings_json);
                std::string warn;
                json_into_settings(j, params.settings, &warn);
                if (!warn.empty()) g_last_error = "settings warnings: " + warn;
            } catch (std::exception const& e) {
                set_err(LT_ERR_PARSE, std::string("settings json parse: ") + e.what());
                return 0;
            }
        }

        // If Go has registered storage callbacks, swap in our custom
        // disk_io constructor before the session boots.
        tsl_install_disk_io_on(params);

        auto slot = std::make_shared<session_slot>();
        slot->s = std::make_unique<lt::session>(std::move(params));

        int64_t id = g_next_sess++;
        {
            std::unique_lock<std::shared_mutex> lk(g_sess_mu);
            g_sessions.emplace(id, slot);
        }
        return id;
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

int lt_session_apply_settings(lt_session id, const char* settings_json) {
    WRAP_BEGIN
    auto slot = get_session(id);
    if (!slot) return set_err(LT_ERR_NOT_FOUND, "session not found");
    if (!settings_json || !*settings_json) return LT_OK;

    json j;
    try { j = json::parse(settings_json); }
    catch (std::exception const& e) { return set_err(LT_ERR_PARSE, e.what()); }

    lt::settings_pack sp;
    std::string warn;
    json_into_settings(j, sp, &warn);
    slot->s->apply_settings(std::move(sp));
    if (!warn.empty()) g_last_error = "settings warnings: " + warn;
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_session_set_ip_filter(lt_session id, const char* p2p_text) {
    WRAP_BEGIN
    auto slot = get_session(id);
    if (!slot) return set_err(LT_ERR_NOT_FOUND, "session not found");
    std::string txt = p2p_text ? p2p_text : "";
    slot->s->set_ip_filter(parse_p2p_filter(txt));
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_session_set_alert_mask(lt_session id, uint32_t mask) {
    WRAP_BEGIN
    auto slot = get_session(id);
    if (!slot) return set_err(LT_ERR_NOT_FOUND, "session not found");
    if (mask == 0) mask = LT_ALERT_DEFAULT;
    lt::settings_pack sp;
    sp.set_int(lt::settings_pack::alert_mask, static_cast<int>(mask));
    slot->s->apply_settings(std::move(sp));
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_session_destroy(lt_session id) {
    WRAP_BEGIN
    std::shared_ptr<session_slot> slot;
    {
        std::unique_lock<std::shared_mutex> lk(g_sess_mu);
        auto it = g_sessions.find(id);
        if (it == g_sessions.end()) return set_err(LT_ERR_NOT_FOUND, "session not found");
        slot = std::move(it->second);
        g_sessions.erase(it);
    }
    // Forget all torrents — we only ever have one session at a time.
    {
        std::unique_lock<std::shared_mutex> lk(g_torr_mu);
        for (auto it = g_torrents.begin(); it != g_torrents.end();) {
            if (it->second.is_valid()) {
                g_hash2id.erase(sha1_hex(it->second.info_hashes().v1));
            }
            it = g_torrents.erase(it);
        }
    }
    slot.reset();
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

// ----- torrent lifecycle -----

lt_torrent lt_session_add_torrent(
    lt_session sid,
    const char* link,
    const uint8_t* info_bytes, size_t info_len,
    const char* trackers_csv,
    const char* save_path,
    int paused)
{
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto slot = get_session(sid);
        if (!slot) { set_err(LT_ERR_NOT_FOUND, "session not found"); return 0; }

        lt::add_torrent_params atp;
        atp.save_path = save_path ? save_path : ".";

        if (info_bytes && info_len > 0) {
            lt::error_code ec;
            atp.ti = std::make_shared<lt::torrent_info>(
                reinterpret_cast<char const*>(info_bytes), static_cast<int>(info_len), ec);
            if (ec) { set_err_ec(LT_ERR_PARSE, ec); return 0; }
        } else if (link && *link) {
            std::string l(link);
            if (l.size() == 40) {
                lt::sha1_hash h;
                if (hex_to_sha1(l, h)) {
                    atp.info_hashes.v1 = h;
                } else {
                    lt::error_code ec;
                    lt::parse_magnet_uri(l, atp, ec);
                    if (ec) { set_err_ec(LT_ERR_PARSE, ec); return 0; }
                }
            } else if (l.rfind("magnet:", 0) == 0) {
                lt::error_code ec;
                lt::parse_magnet_uri(l, atp, ec);
                if (ec) { set_err_ec(LT_ERR_PARSE, ec); return 0; }
            } else {
                set_err(LT_ERR_NOT_IMPL, "http/file links must be pre-fetched");
                return 0;
            }
        } else {
            set_err(LT_ERR_INVALID, "link and info_bytes are both empty");
            return 0;
        }

        if (trackers_csv && *trackers_csv) {
            for (auto& t : split_csv(trackers_csv)) {
                atp.trackers.push_back(t);
            }
        }

        if (paused) {
            atp.flags |= lt::torrent_flags::paused;
            atp.flags &= ~lt::torrent_flags::auto_managed;
        } else {
            atp.flags &= ~lt::torrent_flags::paused;
            atp.flags |= lt::torrent_flags::auto_managed;
        }

        lt::error_code ec;
        lt::torrent_handle h = slot->s->add_torrent(std::move(atp), ec);
        if (ec) { set_err_ec(LT_ERR_INTERNAL, ec); return 0; }
        if (!h.is_valid()) { set_err(LT_ERR_INTERNAL, "invalid handle"); return 0; }

        return register_torrent(h);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

int lt_torrent_remove(lt_session sid, lt_torrent tid, int delete_files) {
    WRAP_BEGIN
    auto slot = get_session(sid);
    if (!slot) return set_err(LT_ERR_NOT_FOUND, "session not found");
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    auto flags = delete_files ? lt::session::delete_files : lt::remove_flags_t{};
    slot->s->remove_torrent(h, flags);
    unregister_torrent(tid);
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_pause(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    h.pause();
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_resume(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    h.resume();
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_force_recheck(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    h.force_recheck();
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

// ----- metadata accessors -----

int lt_torrent_have_metadata(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    auto ti = h.torrent_file();
    return (ti && ti->is_valid()) ? 1 : 0;
    WRAP_END(LT_ERR_INTERNAL)
}

char* lt_torrent_metadata_alloc(lt_torrent tid, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return nullptr; }
        auto ti = h.torrent_file();
        if (!ti || !ti->is_valid()) { set_err(LT_ERR_NOT_FOUND, "no metadata yet"); return nullptr; }
        auto const& info = ti->info_section();
        std::string s(info.data(), info.size());
        return alloc_string(s, out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

int lt_torrent_num_files(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    auto ti = h.torrent_file();
    if (!ti) return 0;
    return ti->num_files();
    WRAP_END(LT_ERR_INTERNAL)
}

size_t lt_torrent_file_path(lt_torrent tid, int idx, char* buf, size_t cap) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return 0; }
        auto ti = h.torrent_file();
        if (!ti) { set_err(LT_ERR_NOT_FOUND, "no metadata yet"); return 0; }
        auto const& fs = ti->files();
        if (idx < 0 || idx >= fs.num_files()) { set_err(LT_ERR_INVALID, "file index out of range"); return 0; }
        std::string p = fs.file_path(lt::file_index_t{idx});
        return copy_string(p, buf, cap);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

int64_t lt_torrent_file_size(lt_torrent tid, int idx) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return -1; }
        auto ti = h.torrent_file();
        if (!ti) { set_err(LT_ERR_NOT_FOUND, "no metadata yet"); return -1; }
        auto const& fs = ti->files();
        if (idx < 0 || idx >= fs.num_files()) { set_err(LT_ERR_INVALID, "file index out of range"); return -1; }
        return fs.file_size(lt::file_index_t{idx});
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return -1;
    }
}

int64_t lt_torrent_file_offset(lt_torrent tid, int idx) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return -1; }
        auto ti = h.torrent_file();
        if (!ti) { set_err(LT_ERR_NOT_FOUND, "no metadata yet"); return -1; }
        auto const& fs = ti->files();
        if (idx < 0 || idx >= fs.num_files()) { set_err(LT_ERR_INVALID, "file index out of range"); return -1; }
        return fs.file_offset(lt::file_index_t{idx});
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return -1;
    }
}

int lt_torrent_num_pieces(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    auto ti = h.torrent_file();
    if (!ti) return 0;
    return ti->num_pieces();
    WRAP_END(LT_ERR_INTERNAL)
}

int64_t lt_torrent_piece_length(lt_torrent tid) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return -1; }
        auto ti = h.torrent_file();
        if (!ti) return 0;
        return ti->piece_length();
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return -1;
    }
}

int64_t lt_torrent_total_size(lt_torrent tid) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return -1; }
        auto ti = h.torrent_file();
        if (!ti) return 0;
        return ti->total_size();
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return -1;
    }
}

size_t lt_torrent_display_name(lt_torrent tid, char* buf, size_t cap) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return 0; }
        auto st = h.status(lt::torrent_handle::query_name);
        return copy_string(st.name, buf, cap);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

size_t lt_torrent_info_hash_hex(lt_torrent tid, char* buf, size_t cap) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return 0; }
        std::string hex = sha1_hex(h.info_hashes().v1);
        return copy_string(hex, buf, cap);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

// ----- priorities & streaming -----

int lt_torrent_set_piece_priority(lt_torrent tid, int piece_idx, int prio) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    if (prio < 0 || prio > 7) return set_err(LT_ERR_INVALID, "prio out of range");
    h.piece_priority(lt::piece_index_t{piece_idx},
                     static_cast<lt::download_priority_t>(static_cast<std::uint8_t>(prio)));
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_set_piece_deadline(lt_torrent tid, int piece_idx, int deadline_ms, int alert_when_ready) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    lt::deadline_flags_t flags = {};
    if (alert_when_ready) flags |= lt::torrent_handle::alert_when_available;
    h.set_piece_deadline(lt::piece_index_t{piece_idx}, deadline_ms, flags);
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_clear_piece_deadlines(lt_torrent tid) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    h.clear_piece_deadlines();
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

int lt_torrent_set_file_priority(lt_torrent tid, int file_idx, int prio) {
    WRAP_BEGIN
    auto h = get_torrent(tid);
    if (!h.is_valid()) return set_err(LT_ERR_NOT_FOUND, "torrent not found");
    if (prio < 0 || prio > 7) return set_err(LT_ERR_INVALID, "prio out of range");
    h.file_priority(lt::file_index_t{file_idx},
                    static_cast<lt::download_priority_t>(static_cast<std::uint8_t>(prio)));
    return LT_OK;
    WRAP_END(LT_ERR_INTERNAL)
}

// ----- status & stats -----

size_t lt_torrent_status_json(lt_torrent tid, char* buf, size_t cap) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto h = get_torrent(tid);
        if (!h.is_valid()) { set_err(LT_ERR_NOT_FOUND, "torrent not found"); return 0; }
        std::string s = status_to_json(h).dump();
        return copy_string(s, buf, cap);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return 0;
    }
}

char* lt_session_stats_json_alloc(lt_session sid, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto slot = get_session(sid);
        if (!slot) { set_err(LT_ERR_NOT_FOUND, "session not found"); return nullptr; }
        slot->s->post_session_stats();
        json j;
        j["requested"] = true;
        std::string s = j.dump();
        return alloc_string(s, out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

// ----- alert pump -----

int lt_session_wait_alert(lt_session sid, int timeout_ms) {
    WRAP_BEGIN
    auto slot = get_session(sid);
    if (!slot) return set_err(LT_ERR_NOT_FOUND, "session not found");
    std::lock_guard<std::mutex> lk(slot->pump_mu);
    if (timeout_ms < 0) timeout_ms = 1000 * 60 * 60; // cap at 1h
    auto* a = slot->s->wait_for_alert(std::chrono::milliseconds(timeout_ms));
    return a ? 1 : 0;
    WRAP_END(LT_ERR_INTERNAL)
}

char* lt_session_pop_alerts_json_alloc(lt_session sid, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        auto slot = get_session(sid);
        if (!slot) { set_err(LT_ERR_NOT_FOUND, "session not found"); return nullptr; }

        std::vector<lt::alert*> alerts;
        {
            std::lock_guard<std::mutex> lk(slot->pump_mu);
            slot->s->pop_alerts(&alerts);
        }
        json arr = json::array();
        for (auto* a : alerts) {
            try { arr.push_back(alert_to_json(a)); }
            catch (std::exception const&) { /* skip malformed */ }
        }
        std::string s = arr.dump();
        return alloc_string(s, out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

// Storage callbacks are now installed via lt_install_storage_callbacks_full
// (declared in lt_disk_io.h, implemented in lt_disk_io.cpp). The old
// 3-pointer API was removed in Etap 4.

// ----- parsers (utility) -----

static char* parse_atp_to_json(lt::add_torrent_params const& atp, size_t* out_len) {
    json j;
    j["info_hash"] = sha1_hex(atp.info_hashes.v1);
    j["display_name"] = atp.name;
    json tr = json::array();
    for (auto const& t : atp.trackers) tr.push_back(t);
    j["trackers"] = std::move(tr);
    if (atp.ti) {
        auto const& info = atp.ti->info_section();
        j["has_metadata"] = true;
        j["metadata_size"] = static_cast<int>(info.size());
    } else {
        j["has_metadata"] = false;
        j["metadata_size"] = 0;
    }
    std::string s = j.dump();
    return alloc_string(s, out_len);
}

char* lt_parse_magnet_alloc(const char* uri, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        if (!uri || !*uri) { set_err(LT_ERR_INVALID, "empty uri"); return nullptr; }
        lt::add_torrent_params atp;
        lt::error_code ec;
        lt::parse_magnet_uri(uri, atp, ec);
        if (ec) { set_err_ec(LT_ERR_PARSE, ec); return nullptr; }
        return parse_atp_to_json(atp, out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

char* lt_parse_torrent_bytes_alloc(const uint8_t* buf, size_t len, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        if (!buf || len == 0) { set_err(LT_ERR_INVALID, "empty bytes"); return nullptr; }
        lt::error_code ec;
        auto ti = std::make_shared<lt::torrent_info>(
            reinterpret_cast<char const*>(buf), static_cast<int>(len), ec);
        if (ec) { set_err_ec(LT_ERR_PARSE, ec); return nullptr; }
        lt::add_torrent_params atp;
        atp.ti = ti;
        atp.info_hashes = ti->info_hashes();
        atp.name = ti->name();
        for (auto const& tracker : ti->trackers()) {
            atp.trackers.push_back(tracker.url);
        }
        return parse_atp_to_json(atp, out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

char* lt_parse_torrent_file_alloc(const char* path, size_t* out_len) {
    g_last_error.clear(); g_last_error_code = LT_OK;
    try {
        if (!path || !*path) { set_err(LT_ERR_INVALID, "empty path"); return nullptr; }
        std::ifstream f(path, std::ios::binary);
        if (!f) { set_err(LT_ERR_IO, std::string("cannot open ") + path); return nullptr; }
        std::vector<char> buf((std::istreambuf_iterator<char>(f)), {});
        if (buf.empty()) { set_err(LT_ERR_IO, "file is empty"); return nullptr; }
        return lt_parse_torrent_bytes_alloc(
            reinterpret_cast<uint8_t const*>(buf.data()), buf.size(), out_len);
    } catch (std::exception const& e) {
        set_err(LT_ERR_INTERNAL, e.what());
        return nullptr;
    }
}

} // extern "C"
