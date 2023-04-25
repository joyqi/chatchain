import puppeteer from "puppeteer";
import { Readability } from "@mozilla/readability";
import { JSDOM } from "jsdom";
import { convert } from 'html-to-text';
import { SentenceSplitter } from "../splitter";
import { chunkSize } from "../config";
import { Handler } from ".";
import inquirer from "inquirer";
import ora from "ora";

async function fetch(url: string) {
    const browser = await puppeteer.launch({
        headless: "new",
    });
    const page = await browser.newPage();
    await page.goto(url, { waitUntil: "networkidle2" });
    const html = await page.content();
    await browser.close();
    return html;
}

async function scrape(html: string) {
    const doc = new JSDOM(html);
    const reader = new Readability(doc.window.document);
    const article = reader.parse();

    if (article) {
        const content = convert(article.content, {
            wordwrap: false,
            selectors: [
                { selector: 'a', options: { ignoreHref: true } },
                { selector: 'img', format: 'skip' },
                { selector: 'hr', format: 'skip' }
            ]
        });

        const splitter = new SentenceSplitter({ chunkSize });
        return splitter.createDocuments([article.title + "\n" + content]);
    }

    throw new Error("Could not parse article");
}

async function crawl(url: string) {
    const spinner = ora("Fetching URL: " + url).start();
    const html = await fetch(url);
    spinner.stop();

    return await scrape(html);
}

async function promptUrl() {
    const { srcUrl } = await inquirer.prompt([
        {
            type: 'input',
            name: 'srcUrl',
            message: 'Enter URL: ',
            validate: async (input: string) => {
                if (input.length === 0 || !input.match(/^https?:\/\/.+/)) {
                    return 'Please enter a URL';
                }
                return true;
            }
        },
    ]);

    return srcUrl;
}

export default class extends Handler {
    async handle() {
        const url = await promptUrl();
        const docs = await crawl(url);
        await this.store.saveVector(docs);
    }
}
