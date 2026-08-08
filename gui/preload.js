'use strict';

const { contextBridge, ipcRenderer } = require('electron');

// Nur diese Kanäle dürfen per on() abonniert werden (Push vom Main-Prozess).
const ALLOWED_EVENTS = ['mirror:status', 'display:list', 'sidecar:error', 'device:updated', 'audio:status'];

function on(channel, callback) {
  if (!ALLOWED_EVENTS.includes(channel)) {
    throw new Error(`Nicht erlaubter Event-Kanal: ${channel}`);
  }
  const listener = (_event, payload) => callback(payload);
  ipcRenderer.on(channel, listener);
  return () => ipcRenderer.removeListener(channel, listener);
}

contextBridge.exposeInMainWorld('luftspiegel', {
  devices: {
    discover: () => ipcRenderer.invoke('device:discover'),
    list: () => ipcRenderer.invoke('device:list'),
  },
  mirror: {
    start: (payload) => ipcRenderer.invoke('mirror:start', payload),
    stop: (payload) => ipcRenderer.invoke('mirror:stop', payload),
    status: () => ipcRenderer.invoke('mirror:status'),
  },
  displays: {
    list: () => ipcRenderer.invoke('display:list'),
  },
  settings: {
    get: () => ipcRenderer.invoke('settings:get'),
    set: (settings) => ipcRenderer.invoke('settings:set', settings),
  },
  sidecar: {
    lastError: () => ipcRenderer.invoke('sidecar:lastError'),
  },
  audio: {
    // Fire-and-forget: PCM-Block (ArrayBuffer) aus dem AudioWorklet an den
    // Main-Prozess weiterreichen, der ihn auf den TCP-Socket schreibt.
    sendChunk: (arrayBuffer) => ipcRenderer.send('audio:chunk', arrayBuffer),
    // Pull-Fallback für den aktuellen Bridge-Zustand (siehe main.js).
    status: () => ipcRenderer.invoke('audio:statusGet'),
  },
  on,
});
