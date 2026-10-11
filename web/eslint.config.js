import { defineConfig } from "eslint/config";
import globals from "globals";
import tseslint from "typescript-eslint";

// The UI renders model-written markdown, so this config pins down where HTML
// may become DOM: src/components/Prose.tsx, and nowhere else. It is defense
// in depth. The CSP (script-src 'self', no inline) is the boundary, and
// Trusted Types becomes the backstop once the legacy innerHTML writers are
// gone (8z). Lint is syntax, so a key held in a plain variable still gets
// past it; what it does catch is every form a reviewer could plausibly miss:
// reading a sink at all (calls, .call, Reflect.apply, destructuring), a sink
// name built at runtime, a string handed to Reflect, defineProperty or
// setAttribute, spread or variable props, and inline eslint-disable comments,
// which are ignored under src/ so no diff can switch a ban off for itself.
// test/eslint-fixtures/bypass.tsx writes each form once, and
// test/eslint-sinks.test.ts proves every one is reported.

const HTML_NAMES = "innerHTML|outerHTML|srcdoc|srcDoc|dangerouslySetInnerHTML|insertAdjacentHTML|createContextualFragment|setHTMLUnsafe";
// document.write(ln) too. Kept apart because "write" is an ordinary string
// (a token scope), so only member access and destructuring are banned.
const MEMBER_NAMES = `${HTML_NAMES}|write|writeln`;
const ELEMENT_FACTORIES = "h|createElement|cloneElement|jsx|jsxs|jsxDEV";
const PROSE = "Render HTML through <Prose>; it is the one audited sink.";
const NO_HANDLERS = "No inline handlers or srcdoc, and no attribute names built at runtime; use addEventListener.";

// The one thing Prose.tsx may do.
const htmlAttribute = [
  { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: PROSE },
];

// Everything else, banned everywhere under src/, Prose included.
const otherSinks = [
  { selector: "JSXAttribute[name.name=/^(srcdoc|srcDoc)$/]", message: PROSE },
  // A spread can carry dangerouslySetInnerHTML without naming it here.
  { selector: "JSXSpreadAttribute", message: "Pass props by name; a spread can smuggle dangerouslySetInnerHTML past review." },
  { selector: `Property[key.name=/^(${HTML_NAMES})$/]`, message: PROSE },
  { selector: `ObjectPattern > Property[key.name=/^(${MEMBER_NAMES})$/]`, message: PROSE },
  { selector: `Literal[value=/^(${HTML_NAMES})$/]`, message: `${PROSE} (A sink's name as a string reaches it through a computed key.)` },
  { selector: `TemplateElement[value.raw=/^(${HTML_NAMES})$/]`, message: PROSE },
  // Any read of a sink, not only a call or an assignment: el.innerHTML in a
  // destructuring target, el.insertAdjacentHTML.call(...), Reflect.apply(
  // el.insertAdjacentHTML, ...), const w = document.write.
  { selector: `MemberExpression[property.name=/^(${MEMBER_NAMES})$/]`, message: PROSE },
  { selector: `MemberExpression[computed=true][property.value=/^(${MEMBER_NAMES})$/]`, message: PROSE },
  // A computed key built at runtime ("inner" + "HTML", a template, a call)
  // is unreviewable by construction.
  { selector: "MemberExpression[computed=true][property.type=/^(BinaryExpression|TemplateLiteral|CallExpression)$/]", message: "Computed keys built at runtime are unreviewable; name the property." },
  { selector: "CallExpression[callee.object.name='Reflect'][callee.property.name=/^(get|set|has|defineProperty|deleteProperty|getOwnPropertyDescriptor)$/][arguments.1.type!='Literal']", message: "Reflect with a key that is not a literal is unreviewable; name the property." },
  { selector: "CallExpression[callee.object.name='Object'][callee.property.name=/^(defineProperty|defineProperties)$/]", message: "Defining properties on objects at runtime can reach an HTML sink; set them by name." },
  // An on* attribute set from a string is an inline handler the CSP would
  // refuse at best; srcdoc is a whole document.
  { selector: "CallExpression[callee.property.name='setAttribute'][arguments.0.value=/^(on.*|srcdoc)$/i]", message: NO_HANDLERS },
  { selector: "CallExpression[callee.property.name='setAttribute'][arguments.0.type!='Literal']", message: NO_HANDLERS },
  { selector: "CallExpression[callee.property.name='setAttributeNS'][arguments.1.value=/^(on.*|srcdoc)$/i]", message: NO_HANDLERS },
  { selector: "CallExpression[callee.property.name='setAttributeNS'][arguments.1.type!='Literal']", message: NO_HANDLERS },
];

// Props handed to an element factory as a variable, or spread into one, are
// the hyperscript form of a JSX spread. src/main.ts's mount() is the one
// audited place that forwards props it did not write.
const elementProps = [
  { selector: `CallExpression[callee.name=/^(${ELEMENT_FACTORIES})$/][arguments.length>1][arguments.1.type!='ObjectExpression'][arguments.1.type!='Literal']`, message: "Pass element props as an object literal; a variable can carry dangerouslySetInnerHTML." },
  { selector: `CallExpression[callee.property.name=/^(${ELEMENT_FACTORIES})$/][arguments.length>1][arguments.1.type!='ObjectExpression'][arguments.1.type!='Literal']`, message: "Pass element props as an object literal; a variable can carry dangerouslySetInnerHTML." },
  { selector: `CallExpression[callee.name=/^(${ELEMENT_FACTORIES})$/] > ObjectExpression.arguments > SpreadElement`, message: "Pass element props by name; a spread can carry dangerouslySetInnerHTML." },
  { selector: `CallExpression[callee.property.name=/^(${ELEMENT_FACTORIES})$/] > ObjectExpression.arguments > SpreadElement`, message: "Pass element props by name; a spread can carry dangerouslySetInnerHTML." },
];

const restricted = (...lists) => ["error", ...lists.flat()];

// The sink ban, applied to the browser code and to the bypass fixtures that
// prove it. Kept as one block so the two can never disagree.
const sinkBan = (files) => ({
  files,
  linterOptions: { noInlineConfig: true, reportUnusedDisableDirectives: "error" },
  rules: {
    "no-restricted-syntax": restricted(htmlAttribute, otherSinks, elementProps),
    "no-eval": "error",
    "no-new-func": "error",
    "no-script-url": "error",
    // Test helpers (the legacy loader evaluates app.js) never ship.
    "no-restricted-imports": ["error", { patterns: [{ group: ["**/test/**", "**/test"], message: "src/ must not import test code." }] }],
  },
});

export default defineConfig(
  // The build output lives outside web/ (internal/api/ui/dist), so `eslint .`
  // never reaches it or the legacy app.js. The fixtures are broken on purpose
  // and are linted only by the test that expects them to fail.
  { ignores: ["node_modules/", "test/eslint-fixtures/"] },
  ...tseslint.configs.strictTypeChecked,
  {
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
      globals: globals.browser,
    },
  },
  {
    files: ["test/**", "*.config.ts", "eslint.config.js"],
    languageOptions: { globals: globals.node },
  },
  // This file is plain JS in no tsconfig.
  { files: ["eslint.config.js"], ...tseslint.configs.disableTypeChecked },
  sinkBan(["src/**"]),
  // The fixtures are not in any tsconfig; the sink rules are syntactic and
  // need no type information.
  { files: ["test/eslint-fixtures/**"], ...tseslint.configs.disableTypeChecked },
  sinkBan(["test/eslint-fixtures/**"]),
  // The two audited exceptions, each lifting exactly one list.
  { files: ["src/components/Prose.tsx"], rules: { "no-restricted-syntax": restricted(otherSinks, elementProps) } },
  { files: ["src/main.ts"], rules: { "no-restricted-syntax": restricted(htmlAttribute, otherSinks) } },
);
