import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: 'Burrow',
  description:
    'Self-hosted reverse tunnels — expose a local port through a relay you run yourself.',
  base: '/', // custom domain (docs.burrow.com); use '/burrow/' if served from a project page
  cleanUrls: true,
  lastUpdated: true,
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/quickstart' },
      { text: 'Reference', link: '/reference/cli' },
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Guide',
          items: [
            { text: 'Quickstart', link: '/guide/quickstart' },
            { text: 'Installation', link: '/guide/installation' },
            { text: 'Configuration', link: '/guide/configuration' },
          ],
        },
      ],
      '/reference/': [
        {
          text: 'Reference',
          items: [
            { text: 'CLI', link: '/reference/cli' },
            { text: 'HTTP API', link: '/reference/api' },
          ],
        },
      ],
    },
    search: { provider: 'local' },
    socialLinks: [
      { icon: 'github', link: 'https://github.com/ankoehn/burrow' },
    ],
    footer: {
      message: 'Released under the Apache-2.0 License.',
      copyright: 'Copyright © Burrow contributors',
    },
  },
})
