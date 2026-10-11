import { h, render, type ComponentType } from "preact";

// The bridge between the legacy app.js and ported views. This module is a
// <script type="module"> in <head>, so it runs before the deferred legacy
// scripts in <body> and window.kiln exists by the time app.js boots. route()
// asks views.get(head) before its own chain; a hit mounts the component into
// <main> and registers the unmount with onViewCleanup, so the next route()
// tears it down exactly like a legacy view's observers.
//
// No store yet: a view remounts on every route(), so it reads what it needs
// on mount.

// ViewProps is what route() hands a ported view: the hash it was reached by
// (without "#/") and the navigation token of that route, which a view checks
// before committing slow results, as the legacy views do.
export interface ViewProps {
  hash: string;
  nav: number;
}

// How the legacy chrome presents a ported view: the document title, which
// nav entry is current (a data-view value; several routes can share one, as
// ingest/sources/runs do), and which page the tree highlights.
export interface ViewOptions {
  title?: string;
  view?: string;
  activeSlug?: (hash: string) => string | null;
}

export interface PortedView {
  component: ComponentType<ViewProps>;
  title: string;
  view: string;
  activeSlug: (hash: string) => string | null;
}

export interface KilnBridge {
  // A Map, never a plain object: route() looks a URL segment up in it, and
  // "#/constructor" must not find Object.prototype.constructor.
  views: Map<string, PortedView>;
  registerView(name: string, component: ComponentType<ViewProps>, options?: ViewOptions): void;
  mount<P extends object>(el: Element, component: ComponentType<P>, props: P): () => void;
}

const views = new Map<string, PortedView>();

window.kiln = {
  views,
  registerView(name, component, options = {}) {
    // Two ports claiming one route would make which one renders depend on
    // module order.
    if (views.has(name)) throw new Error(`kiln: view ${name} is already registered`);
    views.set(name, {
      component,
      title: options.title ?? name,
      view: options.view ?? name,
      activeSlug: options.activeSlug ?? (() => null),
    });
  },
  mount(el, component, props) {
    render(h(component, props), el);
    return () => { render(null, el); };
  },
};
