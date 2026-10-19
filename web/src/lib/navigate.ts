/** Sends the browser to an address off this page (an app's redirect). Apart so that a test can replace it. */
export function goTo(url: string): void {
  window.location.assign(url);
}
