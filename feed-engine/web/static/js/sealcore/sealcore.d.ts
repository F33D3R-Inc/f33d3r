/* tslint:disable */
/* eslint-disable */

/**
 * Argon2id: derive a 32-byte key (base64) from a backup code + salt.
 */
export function derive_backup_key(code: string, salt_b64: string): string;

/**
 * Generate a fresh X25519 identity keypair (base64).
 */
export function generate_keypair(): string;

/**
 * Generate a random 16-byte salt (base64) for Argon2id / key derivation.
 */
export function generate_salt(): string;

/**
 * Open a sealed message addressed to me. Returns the plaintext.
 */
export function open_message(my_priv_b64: string, eph_pub_b64: string, sealed_b64: string, sealed_nonce_b64: string, body_ct_b64: string, body_nonce_b64: string): string;

/**
 * Seal a plaintext message to a set of recipients. `recipients_json` is
 * `[{"account":"...","pub_b64":"..."}]`. Returns an Envelope JSON.
 */
export function seal_message(recipients_json: string, plaintext: string): string;

/**
 * Unwrap a secret sealed with `wrap_with_key`. Returns base64 of the plaintext.
 */
export function unwrap_with_key(key_b64: string, ct_b64: string, nonce_b64: string): string;

/**
 * Wrap a secret under a 32-byte symmetric key. Returns `{ct_b64, nonce_b64}`.
 */
export function wrap_with_key(key_b64: string, plaintext_b64: string): string;

export type InitInput = RequestInfo | URL | Response | BufferSource | WebAssembly.Module;

export interface InitOutput {
    readonly memory: WebAssembly.Memory;
    readonly derive_backup_key: (a: number, b: number, c: number, d: number) => [number, number, number, number];
    readonly generate_keypair: () => [number, number, number, number];
    readonly generate_salt: () => [number, number];
    readonly open_message: (a: number, b: number, c: number, d: number, e: number, f: number, g: number, h: number, i: number, j: number, k: number, l: number) => [number, number, number, number];
    readonly seal_message: (a: number, b: number, c: number, d: number) => [number, number, number, number];
    readonly unwrap_with_key: (a: number, b: number, c: number, d: number, e: number, f: number) => [number, number, number, number];
    readonly wrap_with_key: (a: number, b: number, c: number, d: number) => [number, number, number, number];
    readonly __wbindgen_exn_store: (a: number) => void;
    readonly __externref_table_alloc: () => number;
    readonly __wbindgen_externrefs: WebAssembly.Table;
    readonly __wbindgen_malloc: (a: number, b: number) => number;
    readonly __wbindgen_realloc: (a: number, b: number, c: number, d: number) => number;
    readonly __externref_table_dealloc: (a: number) => void;
    readonly __wbindgen_free: (a: number, b: number, c: number) => void;
    readonly __wbindgen_start: () => void;
}

export type SyncInitInput = BufferSource | WebAssembly.Module;

/**
 * Instantiates the given `module`, which can either be bytes or
 * a precompiled `WebAssembly.Module`.
 *
 * @param {{ module: SyncInitInput }} module - Passing `SyncInitInput` directly is deprecated.
 *
 * @returns {InitOutput}
 */
export function initSync(module: { module: SyncInitInput } | SyncInitInput): InitOutput;

/**
 * If `module_or_path` is {RequestInfo} or {URL}, makes a request and
 * for everything else, calls `WebAssembly.instantiate` directly.
 *
 * @param {{ module_or_path: InitInput | Promise<InitInput> }} module_or_path - Passing `InitInput` directly is deprecated.
 *
 * @returns {Promise<InitOutput>}
 */
export default function __wbg_init (module_or_path?: { module_or_path: InitInput | Promise<InitInput> } | InitInput | Promise<InitInput>): Promise<InitOutput>;
