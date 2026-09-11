(() => {
  "use strict";

  const supportedLanguages = new Set(["en", "ru", "zh"]);
  const preferenceKey = "kube-dump-language";

  function languageFromPath(pathname) {
    const segment = pathname.split("/").filter(Boolean)[0] || "en";
    return supportedLanguages.has(segment) ? segment : null;
  }

  function saveSelectedLanguage(event) {
    const target = event.target;
    const link = target instanceof Element ? target.closest("a[href]") : null;
    if (!link) {
      return;
    }

    const url = new URL(link.href, window.location.href);
    const language = languageFromPath(url.pathname);
    if (language) {
      window.localStorage.setItem(preferenceKey, language);
    }
  }

  function preferredBrowserLanguage() {
    const languages = window.navigator.languages || [window.navigator.language];
    for (const language of languages) {
      const normalized = language.toLowerCase().split(/[-_]/, 1)[0];
      if (supportedLanguages.has(normalized)) {
        return normalized;
      }
    }
    return "en";
  }

  document.addEventListener("click", saveSelectedLanguage, true);

  const path = window.location.pathname.replace(/\/+$/, "") || "/";
  if (path !== "/" && path !== "/index.html") {
    return;
  }

  const selectedLanguage = window.localStorage.getItem(preferenceKey);
  const language = supportedLanguages.has(selectedLanguage)
    ? selectedLanguage
    : preferredBrowserLanguage();

  if (language !== "en") {
    window.location.replace(`${window.location.origin}/${language}/`);
  }
})();
