import inquirer from "inquirer";
import ora from "ora";
import { Store, getModel } from "../store";
import { ConversationalRetrievalQAChain } from "langchain/chains";
import { chatLang } from "../config";

const qaTemplate = `Use the following pieces of context to answer the question ${chatLang ? `in ${chatLang}` : ''} at the end. If you don't know the answer, just say that you don't know, don't try to make up an answer.
{context}
Question: {question}
Helpful Answer:`;

export abstract class Handler {
    constructor(protected store: Store) { }

    protected getLLMChain() {
        const vectorStore = this.store.getVectorStore();
        return ConversationalRetrievalQAChain.fromLLM(getModel(), vectorStore, { qaTemplate });
    }

    async handle() {
        const history = [];

        while (true) {
            const { question } = await inquirer.prompt([
                {
                    type: 'input',
                    name: 'question',
                    message: 'Enter question: ',
                    validate: async (input: string) => {
                        if (input.length === 0) {
                            return 'Please enter a question';
                        }
                        return true;
                    }
                },
            ]);

            const spinner = ora("Thinking...").start();
            const { text } = await this.getLLMChain().call({ question, chat_history: history });
            history.push(question, text);
            spinner.stop();

            console.log(text);
        }
    }
}

import WebHandler from "./web";
export { WebHandler };