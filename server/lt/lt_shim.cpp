#include "lt_shim.h"

#include <libtorrent/version.hpp>

extern "C" const char* lt_shim_version(void) {
    return LIBTORRENT_VERSION;
}
