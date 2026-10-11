import type { KilnBridge } from "./main";

declare global {
  interface Window {
    kiln: KilnBridge;
  }
}

export {};
