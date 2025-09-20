  Plan for `gom` project:

   1. Initialize Go Modules (if necessary) and Install `gomponents`:
       * Ensure go.mod is correctly set up in /home/gom.
       * Add github.com/maragudk/gomponents and github.com/maragudk/gomponents/html to go.mod and download them.
   2. Integrate Tailwind CSS:
       * Install Node.js and npm (if not already available in the environment).
       * Initialize a Node.js project (npm init -y) in /home/gom.
       * Install tailwindcss and its peer dependencies (postcss, autoprefixer).
       * Generate tailwind.config.js and postcss.config.js.
       * Configure tailwind.config.js to scan Go files for class names.
       * Create an input.css with Tailwind directives.
       * Set up a build script to compile Tailwind CSS.
   3. Refactor `main.go` to use `gomponents`:
       * Modify main.go in /home/gom to use gomponents to render the HTML structure, replacing the static index.html.
       * This will involve creating Go functions that return gomponents.Node for different parts of the UI (e.g., MagnetInputWindow, TorrentListWindow,
         StreamWindow).
       * The staticFiles embed.FS will still be used for favicon.ico and the compiled tailwind.css.
       * The script.js will still be served, but its role will shift to handling client-side interactivity for the server-rendered HTML.
   4. Update `script.js`:
       * Adjust script.js to work with the server-rendered HTML structure. This might involve minor changes to how elements are selected or updated, but
         the core logic for API calls and video playback should remain similar.
   5. Update `style.css` (or remove it):
       * The existing style.css will be replaced by the generated Tailwind CSS. We will likely remove style.css and link to the compiled Tailwind output.
