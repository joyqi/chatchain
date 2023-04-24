import puppeteer from "puppeteer";
import { Readability } from "@mozilla/readability";
import { JSDOM } from "jsdom";
import { convert } from 'html-to-text';
import { RecursiveCharacterTextSplitter } from "langchain/text_splitter";

async function fetch(url: string) {
    const browser = await puppeteer.launch();
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

        const splitter = new RecursiveCharacterTextSplitter();
        return splitter.createDocuments([article.title, content]);
    }

    throw new Error("Could not parse article");
}

export async function crawl(url: string) {
    const html = await fetch(url);
    return await scrape(html);
}
