// Firefox exposes the WebExtensions API as `browser`; Chrome exposes the
// equivalent MV3 API as `chrome`. Both browsers support the promise-based
// methods used by this extension at the minimum versions in manifest.json.
export const extensionAPI = globalThis.browser ?? globalThis.chrome;
