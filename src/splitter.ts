import { TextSplitter } from "langchain/text_splitter";
import { split } from "sentence-splitter";
import { encoding_for_model } from "@dqbd/tiktoken";
import { inMemory } from "node-inmemory";

const [getEncoder] = inMemory(() => encoding_for_model('text-davinci-003'));

function tokenLength(str: string) {
    return getEncoder().encode(str).length;
}

function splitBySize(text: string, length: number) {
    let result = '';
    const paragraphs = text.split(/\n+/);
    let paragraph;

    while (paragraph = paragraphs.shift()) {
        const line = paragraph.trim();

        if (line.length === 0) {
            continue;
        } else if (tokenLength(result) + tokenLength(line) <= length) {
            result += line + "\n";
        } else {
            const sentences = split(line);
            let left = '';

            for (const sentence of sentences) {
                const sentenceLen = tokenLength(sentence.raw);

                if (tokenLength(result) + sentenceLen > length) {
                    if (sentenceLen <= length) {
                        left += sentence.raw;
                    }

                    continue;
                }

                result += sentence.raw;
            }

            if (left.length > 0) {
                paragraphs.unshift(left);
            }

            break;
        }
    }

    return [result.trim(), paragraphs.join("\n")];
}

export class SentenceSplitter extends TextSplitter {
    async splitText(text: string): Promise<string[]> {
        const result = [];
        
        while (true) {
            let [chunk, rest] = splitBySize(text, this.chunkSize);
            
            if (chunk.length !== 0) {
                result.push(chunk);
            }

            if (rest.length === 0) {
                break;
            }

            text = rest;
        }

        return result;
    }
}