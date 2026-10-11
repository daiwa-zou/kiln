// Every way this codebase knows of to reach an HTML or script sink. Each line
// marked BAD must be reported by eslint.config.js; test/eslint-sinks.test.ts
// fails if one is not, or if a line not marked BAD is. Never imported, never
// built, never type-checked.
import { loadLegacy } from "../test/legacy"; // BAD: src may not import tests

declare const el: HTMLElement;
declare const frame: HTMLIFrameElement;
declare const html: string;
declare const props: Record<string, unknown>;
declare const key: string;
declare const attrName: string;
declare const vnode: unknown;
declare function h(type: unknown, props?: unknown): unknown;
declare function createElement(type: unknown, props?: unknown): unknown;
declare function cloneElement(v: unknown, props?: unknown): unknown;

export const a = <div dangerouslySetInnerHTML={{ __html: html }} />; // BAD
export const b = <div {...props} />; // BAD: a spread can carry the attribute
export const c = <iframe srcdoc={html} />; // BAD
export const d = <iframe srcDoc={html} />; // BAD
export const e = { dangerouslySetInnerHTML: { __html: html } }; // BAD
export const f = { ["dangerouslySetInnerHTML"]: { __html: html } }; // BAD
const innerHTML = html;
export const g = Object.assign(el, { innerHTML }); // BAD: shorthand key
el.innerHTML = html; // BAD
el.outerHTML = html; // BAD
el["innerHTML"] = html; // BAD
el[`outerHTML`] = html; // BAD
frame.srcdoc = html; // BAD
Reflect.set(el, "innerHTML", html); // BAD
el.insertAdjacentHTML("beforeend", html); // BAD
document.write(html); // BAD
document.writeln(html); // BAD
export const h = document.createRange().createContextualFragment(html); // BAD
el.setAttribute("onclick", html); // BAD
el.setAttribute("ONLOAD", html); // BAD
frame.setAttribute("srcdoc", html); // BAD
el.setAttributeNS(null, "onmouseover", html); // BAD
eval(html); // BAD
export const i = new Function(html); // BAD
export const j = Function(html); // BAD
export const k = "javascript:void(0)"; // BAD
// Indirections: a sink read rather than called, a name built at runtime.
el["inner" + "HTML"] = html; // BAD
el[`${"inner"}HTML`] = html; // BAD
el["inner".concat("HTML")] = html; // BAD
Reflect.set(el, "inner" + "HTML", html); // BAD
Reflect.get(el, key); // BAD
Object.defineProperty(el, key, { value: html }); // BAD
[el.innerHTML] = [html]; // BAD
for (el.outerHTML of [html]) { /* empty */ } // BAD
el.insertAdjacentHTML.call(el, "beforeend", html); // BAD
Reflect.apply(el.insertAdjacentHTML, el, ["beforeend", html]); // BAD
export const w = document["write"]; // BAD
export const wl = document.writeln; // BAD
export const { write } = document; // BAD
el.setAttribute("on" + "click", html); // BAD
el.setAttribute(attrName, html); // BAD
el.setAttributeNS(null, attrName, html); // BAD
export const m = h("div", props); // BAD: props as a variable
export const n = h("div", { ...props }); // BAD
export const o = createElement("div", props); // BAD
export const p = cloneElement(vnode, props); // BAD
// eslint-disable-next-line no-restricted-syntax
el.innerHTML = html; // BAD: inline config is ignored under the sink ban
/* eslint-disable */
el.outerHTML = html; // BAD

// The sanctioned forms, which must stay quiet.
el.textContent = html;
el.setAttribute("aria-label", html);
el.addEventListener("click", () => loadLegacy);
export const ok = <div class="prose" title={html} />;
export const ok2 = h("div", { class: "prose", title: html });
export const ok3 = h("div", null);
export const ok5 = h("div");
export const ok4 = el.dataset[key];
