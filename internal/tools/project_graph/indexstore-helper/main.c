#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdbool.h>
#include <Block.h>
#include <sys/stat.h>
#include <sys/types.h>

// ---------------------------------------------------------------------------
// Manual function declarations from CIndexStoreDB.h
// ---------------------------------------------------------------------------

typedef void *indexstoredb_object_t;
typedef indexstoredb_object_t indexstoredb_index_t;
typedef indexstoredb_object_t indexstoredb_indexstore_library_t;
typedef indexstoredb_object_t indexstoredb_symbol_t;
typedef indexstoredb_object_t indexstoredb_symbol_occurrence_t;
typedef indexstoredb_object_t indexstoredb_error_t;
typedef indexstoredb_object_t indexstoredb_symbol_location_t;
typedef indexstoredb_object_t indexstoredb_creation_options_t;

typedef enum {
    INDEXSTOREDB_SYMBOL_ROLE_DECLARATION = 1 << 0,
    INDEXSTOREDB_SYMBOL_ROLE_DEFINITION  = 1 << 1,
    INDEXSTOREDB_SYMBOL_ROLE_REFERENCE   = 1 << 2,
    INDEXSTOREDB_SYMBOL_ROLE_CANONICAL   = 1ULL << 63,
} indexstoredb_symbol_role_t;

typedef bool (^occurrence_receiver_t)(indexstoredb_symbol_occurrence_t);
typedef indexstoredb_indexstore_library_t (^library_provider_t)(const char *);
typedef void (^delegate_event_receiver_t)(void *);

indexstoredb_creation_options_t indexstoredb_creation_options_create(void);
void indexstoredb_creation_options_dispose(indexstoredb_creation_options_t);
void indexstoredb_creation_options_wait(indexstoredb_creation_options_t, bool wait);

indexstoredb_index_t indexstoredb_index_create(
    const char *storePath, const char *databasePath,
    library_provider_t libProvider, delegate_event_receiver_t delegate,
    indexstoredb_creation_options_t options, indexstoredb_error_t *error);

void indexstoredb_index_poll_for_unit_changes_and_wait(indexstoredb_index_t, bool isInitialScan);
indexstoredb_indexstore_library_t indexstoredb_load_indexstore_library(const char *, indexstoredb_error_t *);

bool indexstoredb_index_canonical_symbol_occurences_by_name(indexstoredb_index_t, const char *, occurrence_receiver_t);
bool indexstoredb_index_canonical_symbol_occurences_containing_pattern(
    indexstoredb_index_t, const char *, bool anchorStart, bool anchorEnd,
    bool subsequence, bool ignoreCase, occurrence_receiver_t);
bool indexstoredb_index_symbol_occurrences_by_usr(indexstoredb_index_t, const char *, uint64_t, occurrence_receiver_t);

indexstoredb_symbol_t indexstoredb_symbol_occurrence_symbol(indexstoredb_symbol_occurrence_t);
uint64_t indexstoredb_symbol_occurrence_roles(indexstoredb_symbol_occurrence_t);
indexstoredb_symbol_location_t indexstoredb_symbol_occurrence_location(indexstoredb_symbol_occurrence_t);

const char *indexstoredb_symbol_name(indexstoredb_symbol_t);
const char *indexstoredb_symbol_usr(indexstoredb_symbol_t);
unsigned indexstoredb_symbol_kind(indexstoredb_symbol_t);

const char *indexstoredb_symbol_location_path(indexstoredb_symbol_location_t);
int indexstoredb_symbol_location_line(indexstoredb_symbol_location_t);
int indexstoredb_symbol_location_column_utf8(indexstoredb_symbol_location_t);

const char *indexstoredb_error_get_description(indexstoredb_error_t);
void indexstoredb_error_dispose(indexstoredb_error_t);

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

static const char *kind_string(unsigned kind) {
    switch (kind) {
    case 5: return "enum"; case 6: return "struct"; case 7: return "class";
    case 8: return "protocol"; case 9: return "extension"; case 11: return "typealias";
    case 12: return "function"; case 13: return "variable";
    case 16: return "instancemethod"; case 17: return "classmethod";
    case 18: return "staticmethod"; case 19: return "instanceproperty";
    case 20: return "classproperty"; case 21: return "staticproperty";
    case 22: return "constructor"; case 25: return "parameter";
    default: return "symbol";
    }
}

static const char *roles_string(uint64_t roles) {
    if (roles & INDEXSTOREDB_SYMBOL_ROLE_DEFINITION) return "definition";
    if (roles & INDEXSTOREDB_SYMBOL_ROLE_DECLARATION) return "declaration";
    if (roles & INDEXSTOREDB_SYMBOL_ROLE_REFERENCE) return "reference";
    return "reference";
}

static void json_escape(const char *src, char *dst, size_t sz) {
    size_t j = 0;
    for (size_t i = 0; src[i] && j < sz - 2; i++) {
        char c = src[i];
        if (c == '"' || c == '\\') { dst[j++] = '\\'; dst[j++] = c; }
        else if (c == '\n') { dst[j++] = '\\'; dst[j++] = 'n'; }
        else if (c == '\t') { dst[j++] = '\\'; dst[j++] = 't'; }
        else dst[j++] = c;
    }
    dst[j] = '\0';
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

static int do_find_symbol(indexstoredb_index_t db, const char *name) {
    printf("[\n");
    __block int first = 1;
    __block int count = 0;

    // Shared callback: prints one symbol as JSON.
    occurrence_receiver_t printSym = ^bool(indexstoredb_symbol_occurrence_t occ) {
        uint64_t roles = indexstoredb_symbol_occurrence_roles(occ);
        if (!(roles & (INDEXSTOREDB_SYMBOL_ROLE_DECLARATION | INDEXSTOREDB_SYMBOL_ROLE_DEFINITION))) return true;

        indexstoredb_symbol_t sym = indexstoredb_symbol_occurrence_symbol(occ);
        indexstoredb_symbol_location_t loc = indexstoredb_symbol_occurrence_location(occ);

        const char *n = indexstoredb_symbol_name(sym);
        const char *u = indexstoredb_symbol_usr(sym);
        const char *f = indexstoredb_symbol_location_path(loc);
        int l = indexstoredb_symbol_location_line(loc);
        unsigned k = indexstoredb_symbol_kind(sym);

        char nb[512], fb[1024];
        json_escape(n, nb, sizeof(nb));
        json_escape(f, fb, sizeof(fb));

        if (!first) printf(",\n");
        first = 0;
        printf("  {\"name\":\"%s\",\"usr\":\"%s\",\"kind\":\"%s\",\"file\":\"%s\",\"line\":%d}",
               nb, u, kind_string(k), fb, l);
        count++;
        return true;
    };

    // Try exact name match first.
    indexstoredb_index_canonical_symbol_occurences_by_name(db, name, printSym);

    // If nothing found, try case-insensitive substring match.
    if (count == 0) {
        indexstoredb_index_canonical_symbol_occurences_containing_pattern(
            db, name, false, false, false, true, printSym);
    }

    printf("\n]\n");
    return 0;
}

// All non-relation roles: declaration + definition + reference + read + write + call etc.
#define ALL_ROLES ((1ULL << 9) - 1)

static int do_occurrences(indexstoredb_index_t db, const char *usr) {
    printf("[\n");
    __block int first = 1;
    bool ok = indexstoredb_index_symbol_occurrences_by_usr(db, usr, ALL_ROLES,
        ^bool(indexstoredb_symbol_occurrence_t occ) {
            indexstoredb_symbol_t sym = indexstoredb_symbol_occurrence_symbol(occ);
            indexstoredb_symbol_location_t loc = indexstoredb_symbol_occurrence_location(occ);

            const char *n = indexstoredb_symbol_name(sym);
            const char *u = indexstoredb_symbol_usr(sym);
            const char *f = indexstoredb_symbol_location_path(loc);
            int l = indexstoredb_symbol_location_line(loc);
            int c = indexstoredb_symbol_location_column_utf8(loc);
            uint64_t r = indexstoredb_symbol_occurrence_roles(occ);
            unsigned k = indexstoredb_symbol_kind(sym);

            char nb[512], fb[1024];
            json_escape(n, nb, sizeof(nb));
            json_escape(f, fb, sizeof(fb));

            if (!first) printf(",\n");
            first = 0;
            printf("  {\"name\":\"%s\",\"usr\":\"%s\",\"kind\":\"%s\",\"file\":\"%s\",\"line\":%d,\"column\":%d,\"roles\":\"%s\"}",
                   nb, u, kind_string(k), fb, l, c, roles_string(r));
            return true;
        });
    printf("\n]\n");
    return ok ? 0 : 1;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

int main(int argc, char *argv[]) {
    if (argc < 4) {
        fprintf(stderr, "Usage: indexstore-helper <find-symbol|occurrences> <indexStorePath> <query> [--db-path <path>]\n");
        return 1;
    }
    const char *command = argv[1];
    const char *storePath = argv[2];
    const char *query = argv[3];
    const char *dbPath = NULL;
    for (int i = 4; i < argc; i++) {
        if (strcmp(argv[i], "--db-path") == 0 && i + 1 < argc)
            dbPath = argv[++i];
    }
    // Default db path: create a temp dir under the project's .build
    char defaultDb[1024] = {0};
    if (!dbPath) {
        snprintf(defaultDb, sizeof(defaultDb), "%s/../indexstore-db", storePath);
        mkdir(defaultDb, 0755);
        dbPath = defaultDb;
    }

    indexstoredb_creation_options_t opts = indexstoredb_creation_options_create();
    indexstoredb_creation_options_wait(opts, true);

    indexstoredb_error_t error = NULL;
    indexstoredb_index_t db = indexstoredb_index_create(
        storePath, dbPath,
        ^indexstoredb_indexstore_library_t(const char *dylibPath) {
            // Try known locations for libIndexStore.dylib
            const char *paths[] = {
                "/Library/Developer/CommandLineTools/usr/lib/libIndexStore.dylib",
                "/Applications/Xcode.app/Contents/Developer/Toolchains/XcodeDefault.xctoolchain/usr/lib/libIndexStore.dylib",
                dylibPath,
                NULL
            };
            for (int i = 0; paths[i]; i++) {
                indexstoredb_indexstore_library_t lib = indexstoredb_load_indexstore_library(paths[i], NULL);
                if (lib) return lib;
            }
            return (indexstoredb_indexstore_library_t)NULL;
        },
        ^void(void *event) { },
        opts, &error);

    indexstoredb_creation_options_dispose(opts);

    if (!db) {
        const char *msg = error ? indexstoredb_error_get_description(error) : "unknown error";
        fprintf(stderr, "Error: could not open index store at %s: %s\n", storePath, msg);
        if (error) indexstoredb_error_dispose(error);
        return 1;
    }

    indexstoredb_index_poll_for_unit_changes_and_wait(db, true);

    int ret = 0;
    if (strcmp(command, "find-symbol") == 0)
        ret = do_find_symbol(db, query);
    else if (strcmp(command, "occurrences") == 0)
        ret = do_occurrences(db, query);
    else {
        fprintf(stderr, "Unknown command: %s\n", command);
        ret = 1;
    }
    return ret;
}
